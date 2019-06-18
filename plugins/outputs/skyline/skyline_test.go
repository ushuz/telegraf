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
  OK = "[{{ .Now }}] GOOD: {{ .Monitor.Name }} {{ .Alert.Formula }}"
  ALERT = "[{{ .Now }}] SHIT: {{ .Monitor.Name }} {{ .Alert.Formula }}"

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

func TestSkyline(t *testing.T) {
	ts := httptest.NewServer(http.NotFoundHandler())
	defer ts.Close()

	u, err := url.Parse(fmt.Sprintf("http://%s", ts.Listener.Addr().String()))
	require.NoError(t, err)

	plugin := &Skyline{}
	toml.Unmarshal([]byte(config), plugin)
	plugin.URL = u.String()

	err = plugin.Connect()
	require.NoError(t, err)

	done := make(chan bool)

	// 3+3 > 5: OK -> ALERT
	ts.Config.Handler = http.HandlerFunc(AssertRequestBodyContains(t, "SHIT: www status_504 > 5", done))
	err = plugin.Write([]telegraf.Metric{getMetric2(), getMetric2()})
	require.NoError(t, err)

	<-done

	// 3+3 > 5: ALERT -> ALERT
	ts.Config.Handler = http.HandlerFunc(AssertRequestBodyContains(t, "SHIT: www status_504 > 5", done))
	err = plugin.Write([]telegraf.Metric{getMetric2(), getMetric2()})
	require.NoError(t, err)

	<-done

	//   3 < 5: ALERT -> OK
	ts.Config.Handler = http.HandlerFunc(AssertRequestBodyContains(t, "GOOD: www status_504 > 5", done))
	err = plugin.Write([]telegraf.Metric{})
	require.NoError(t, err)

	<-done

	m := new(Mocked)
	ts.Config.Handler = http.HandlerFunc(m.RequestHandler)

	// unmatched
	err = plugin.Write([]telegraf.Metric{getMetric1()})
	require.NoError(t, err)
	m.AssertNumberOfCalls(t, "RequestHandler", 0)

	//   3 < 5: OK -> OK
	err = plugin.Write([]telegraf.Metric{getMetric2()})
	require.NoError(t, err)
	m.AssertNumberOfCalls(t, "RequestHandler", 0)

	// .9 > .8: OK -> ALERT
	ts.Config.Handler = http.HandlerFunc(AssertRequestBodyContains(t, "SHIT: www rt_p95 > 0.8", done))
	err = plugin.Write([]telegraf.Metric{getMetric3()})
	require.NoError(t, err)

	<-done
}

type Mocked struct {
	mock.Mock
}

func (m *Mocked) RequestHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
