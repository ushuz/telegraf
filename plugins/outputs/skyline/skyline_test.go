package skyline

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/metric"
	"github.com/influxdata/toml"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var config = `
## URL is the address to send alerts to
url = ""

## Timeout for HTTP message
# timeout = "5s"

## Alert message template
[template]
  OK = "[{{ .Now }}] GOOD: {{ .Monitor.Name }} [{{ .Alert.Formula }}] [{{ .EvaluatedFormula }}]"
  ALERT = "[{{ .Now }}] SHIT: {{ .Monitor.Name }} [{{ .Alert.Formula }}] [{{ .EvaluatedFormula }}] ({{ .Alert.Count }}/{{ .Alert.Threshold }})"

[[monitors]]
  name = "www"
  host = "www.xiachufang.com"
  # uri = "."
  alerts = [
	"status_504 > 5",
	"rt_p95 > 0.8",
  ]
`

func getMetric1() telegraf.Metric {
	m, err := metric.New(
		"accesslog2",
		map[string]string{
			"host":   "m.xiachufang.com",
			"uri":    "/",
			"status": "504",
		},
		map[string]interface{}{
			"rt_count": 3,
			"rt_p95":   1.0,
		},
		time.Unix(0, 0),
	)
	if err != nil {
		panic(err)
	}
	return m
}

func getMetric2() telegraf.Metric {
	m, err := metric.New(
		"accesslog2",
		map[string]string{
			"host":   "www.xiachufang.com",
			"uri":    "/",
			"status": "504",
		},
		map[string]interface{}{
			"rt_count": 3,
			"rt_p95":   1.0,
		},
		time.Unix(1, 0),
	)
	if err != nil {
		panic(err)
	}
	return m
}

func getMetric3() telegraf.Metric {
	m, err := metric.New(
		"accesslog2",
		map[string]string{
			"host":   "www.xiachufang.com",
			"uri":    "/",
			"status": "204",
		},
		map[string]interface{}{
			"rt_count": 10,
			"rt_p95":   0.9,
		},
		time.Unix(2, 0),
	)
	if err != nil {
		panic(err)
	}
	return m
}

func AssertRequestBodyContains(t *testing.T, contains string, done chan bool) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		body, _ := ioutil.ReadAll(r.Body)
		bodyStr := string(body[:])
		w.WriteHeader(http.StatusOK)
		fmt.Println(bodyStr)
		done <- assert.Contains(t, bodyStr, contains)
	}
}

func setup(t *testing.T) (*Skyline, *httptest.Server, func()) {
	ts := httptest.NewServer(http.NotFoundHandler())

	u, err := url.Parse(fmt.Sprintf("http://%s", ts.Listener.Addr().String()))
	require.NoError(t, err)

	plugin := &Skyline{}
	toml.Unmarshal([]byte(config), plugin)
	plugin.URL = u.String()

	err = plugin.Connect()
	require.NoError(t, err)

	return plugin, ts, ts.Close
}

func TestSkyline(t *testing.T) {
	plugin, ts, close := setup(t)
	defer close()

	var err error

	m := new(Mocked)
	ts.Config.Handler = http.HandlerFunc(m.RequestHandler)

	// unmatched
	err = plugin.Write([]telegraf.Metric{getMetric1()})
	require.NoError(t, err)
	m.AssertNumberOfCalls(t, "RequestHandler", 0)

	// 3 < 5: OK -> OK
	err = plugin.Write([]telegraf.Metric{getMetric2()})
	require.NoError(t, err)
	m.AssertNumberOfCalls(t, "RequestHandler", 0)

	// 1st 6 < 5: OK -> OK (Count=1)
	err = plugin.Write([]telegraf.Metric{getMetric2(), getMetric2()})
	require.NoError(t, err)
	m.AssertNumberOfCalls(t, "RequestHandler", 0)

	done := make(chan bool)

	// 2nd 6 > 5: OK (Count=1) -> ALERT
	ts.Config.Handler = http.HandlerFunc(AssertRequestBodyContains(t, "SHIT: www [status_504 > 5] [status_504(6) > 5] (2/2)", done))
	err = plugin.Write([]telegraf.Metric{getMetric2(), getMetric2()})
	require.NoError(t, err)

	<-done

	// 3rd 6 > 5: ALERT -> ALERT
	ts.Config.Handler = http.HandlerFunc(AssertRequestBodyContains(t, "SHIT: www [status_504 > 5] [status_504(6) > 5] (3/2)", done))
	err = plugin.Write([]telegraf.Metric{getMetric2(), getMetric2()})
	require.NoError(t, err)

	<-done

	// 3 < 5: ALERT -> OK
	ts.Config.Handler = http.HandlerFunc(AssertRequestBodyContains(t, "GOOD: www [status_504 > 5] [status_504 > 5]", done))
	err = plugin.Write([]telegraf.Metric{})
	require.NoError(t, err)

	<-done

	// 2x .9 > .8: OK -> ALERT
	ts.Config.Handler = http.HandlerFunc(AssertRequestBodyContains(t, "SHIT: www [rt_p95 > 0.8] [rt_p95(0.9) > 0.8]", done))
	err = plugin.Write([]telegraf.Metric{getMetric3()})
	err = plugin.Write([]telegraf.Metric{getMetric3()})
	err = plugin.Write([]telegraf.Metric{getMetric3()})
	err = plugin.Write([]telegraf.Metric{getMetric3()})
	err = plugin.Write([]telegraf.Metric{getMetric3()})
	require.NoError(t, err)

	<-done
	<-done
	<-done
	<-done
}

type Mocked struct {
	mock.Mock
}

func (m *Mocked) RequestHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
