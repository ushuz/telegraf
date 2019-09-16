package skyline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/Knetic/govaluate"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	"github.com/influxdata/telegraf/plugins/inputs/statsd"
	"github.com/influxdata/telegraf/plugins/outputs"
)

var sampleConfig = `
  ## URL is the address to send alerts to
  url = "http://127.0.0.1:8080/alert"

  ## Timeout for HTTP message
  # timeout = "5s"

  ## Alert message template
  # [outputs.skyline.template]
  #   OK = "[{{ .Now }}] OK: {{ .Monitor.Name }} [{{ .EvaluatedFormula }}]"
  #   ALERT = "[{{ .Now }}] WARN: {{ .Monitor.Name }} [{{ .EvaluatedFormula }}]"

  ## Configuration for monitors and alerts
  [[outputs.skyline.monitors]]
    name = "www"
    host = "www.xiachufang.com"
    # uri = "."
    alerts = [
      "status_500 > 50",
      "status_502 > 20",
      "status_504 > 50",
      "rt_p95 > 0.8",
    ]
`

const (
	defaultAlertThreshold = 2
	defaultClientTimeout  = 5 * time.Second
	defaultContentType    = "text/plain; charset=utf-8"
	defaultTemplateOK     = "[{{ .Now }}] OK: {{ .Monitor.Name }} [{{ .EvaluatedFormula }}]"
	defaultTemplateALERT  = "[{{ .Now }}] WARN: {{ .Monitor.Name }} [{{ .EvaluatedFormula }}]"
)

func getFloat(v interface{}) (float64, error) {
	switch i := v.(type) {
	case float64:
		return i, nil
	case float32:
		return float64(i), nil
	case int64:
		return float64(i), nil
	case int32:
		return float64(i), nil
	case int:
		return float64(i), nil
	case uint64:
		return float64(i), nil
	case uint32:
		return float64(i), nil
	case uint:
		return float64(i), nil
	case string:
		return strconv.ParseFloat(i, 64)
	default:
		return math.NaN(), errors.New("Non-numeric type could not be converted to float")
	}
}

func shortenNumber(v interface{}) string {
	fv := v.(float64)
	if fv < 1000 {
		return fmt.Sprintf("%v", v)
	}
	exp := uint(math.Log(fv) / math.Log(1000))
	return fmt.Sprintf("%.1f%c", fv/math.Pow(1000, float64(exp)), "kmgtpe"[exp-1])
}

// Alert holds alert formula and alerting state
type Alert struct {
	Formula   string
	Threshold uint

	IsAlerting bool
	Count      uint

	expression *govaluate.EvaluableExpression
}

// Evaluate returns formula evaluation result
func (a *Alert) Evaluate(params map[string]interface{}) string {
	s := a.Formula
	for _, v := range a.expression.Vars() {
		val, ok := params[v]
		if ok {
			s = strings.ReplaceAll(s, v, fmt.Sprintf("%s(%v)", v, shortenNumber(val)))
		}
	}
	result, err := a.expression.Evaluate(params)
	if err != nil || !result.(bool) {
		// not triggered
		a.Count = 0
	} else {
		// triggered
		a.Count++
	}
	return s
}

// Monitor monitors a group endpoints filtered by host and uri
type Monitor struct {
	Name   string   `toml:"name"`
	Host   string   `toml:"host"`
	URI    string   `toml:"uri"`
	Alerts []string `toml:"alerts"`

	regexpHost *regexp.Regexp
	regexpURI  *regexp.Regexp
	fields     map[string]statsd.RunningStats
	alerts     map[string]*Alert
}

func (m *Monitor) addField(key string, value interface{}) error {
	v64, err := getFloat(value)
	if err != nil {
		return err
	}
	rs, ok := m.fields[key]
	if !ok {
		rs = statsd.RunningStats{}
	}
	rs.AddValue(v64)
	m.fields[key] = rs
	return nil
}

// Init initializes regexp, fields and alerts of the monitor
func (m *Monitor) Init() {
	// initialize regexp
	m.regexpHost = regexp.MustCompile(m.Host)
	m.regexpURI = regexp.MustCompile(m.URI)

	// reset fields
	m.resetFields()

	// initialize alerts
	alerts := make(map[string]*Alert)
	for _, formula := range m.Alerts {
		expr, err := govaluate.NewEvaluableExpression(formula)
		if err != nil {
			panic(err.Error())
		}
		alerts[formula] = &Alert{
			Formula:    formula,
			Threshold:  defaultAlertThreshold,
			expression: expr,
		}
	}
	m.alerts = alerts
}

// ProcessMetric filters and aggregates each metric for the monitor
func (m *Monitor) ProcessMetric(metric telegraf.Metric) error {
	host, ok := metric.GetTag("host")
	if !ok {
		return fmt.Errorf("skyline: metric has no 'host' tag")
	}
	uri, ok := metric.GetTag("uri")
	if !ok {
		return fmt.Errorf("skyline: metric has no 'uri' tag")
	}

	// skip unmatched metric
	if !m.regexpHost.MatchString(host) || !m.regexpURI.MatchString(uri) {
		return nil
	}

	// get status code
	status, ok := metric.GetTag("status")
	if !ok {
		return fmt.Errorf("skyline: metric has no 'status' tag")
	}
	statusInt, err := strconv.ParseInt(status, 0, 64)
	if err != nil {
		return fmt.Errorf("skyline: metric status is not a numeric value")
	}

	if 200 <= statusInt && statusInt < 300 {
		// record 2xx request time
		requestTime, ok := metric.GetField("rt_p95")
		if !ok {
			return fmt.Errorf("skyline: metric has no 'rt_p95' field")
		}
		m.addField("rt_p95", requestTime)
	} else if statusInt >= 400 {
		// record 4xx + 5xx request count
		requestCount, ok := metric.GetField("rt_count")
		if !ok {
			return fmt.Errorf("skyline: metric has no 'rt_count' field")
		}
		m.addField("status_"+status, requestCount)
	}

	return nil
}

func (m *Monitor) resetFields() {
	m.fields = make(map[string]statsd.RunningStats)
}

// ShowAlerts returns triggered alert messages of the monitor
func (m *Monitor) ShowAlerts(template *TemplateConfig) []string {
	// map fields to params for evaluation
	params := make(map[string]interface{})
	for field, stats := range m.fields {
		if strings.HasPrefix(field, "status_") {
			params[field] = stats.Sum()
		} else if strings.Contains(field, "rt_") {
			params[field] = stats.Percentile(80)
		}
	}
	// evaluate each alert
	var outputs []string
	for _, alert := range m.alerts {
		evalueatedFormula := alert.Evaluate(params)
		if alert.Count >= alert.Threshold {
			alert.IsAlerting = true
			outputs = append(outputs, RenderTemplate(template.tALERT, m, alert, evalueatedFormula))
		} else if alert.IsAlerting {
			alert.IsAlerting = false
			outputs = append(outputs, RenderTemplate(template.tOK, m, alert, evalueatedFormula))
		}
	}
	// reset fields
	m.resetFields()
	// return alerts text to send to URL
	return outputs
}

// TemplateMessage abstracts properties needed for template rendering
type TemplateMessage struct {
	Now              string
	Monitor          *Monitor
	Alert            *Alert
	EvaluatedFormula string
}

// RenderTemplate renders alert template based on monitor and alert
func RenderTemplate(tpl *template.Template, monitor *Monitor, alert *Alert, evaluatedFormula string) string {
	now := time.Now().Format(time.RFC3339)
	msg := &TemplateMessage{
		Now:              now,
		Monitor:          monitor,
		Alert:            alert,
		EvaluatedFormula: evaluatedFormula,
	}
	buf := &bytes.Buffer{}
	tpl.Execute(buf, msg)
	return buf.String()
}

// TemplateConfig holds customized alert templates
type TemplateConfig struct {
	OK    string `toml:"OK"`
	ALERT string `toml:"ALERT"`

	tOK    *template.Template
	tALERT *template.Template
}

// Skyline is a plugin that send access log alerts over HTTP
type Skyline struct {
	URL      string            `toml:"url"`
	Timeout  internal.Duration `toml:"timeout"`
	Template *TemplateConfig   `toml:"template"`
	Monitors []*Monitor        `toml:"monitors"`

	client *http.Client
}

func (s *Skyline) createClient(ctx context.Context) (*http.Client, error) {
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
		},
		Timeout: s.Timeout.Duration,
	}
	return client, nil
}

// Connect to the Output
func (s *Skyline) Connect() error {
	if s.Timeout.Duration == 0 {
		s.Timeout.Duration = defaultClientTimeout
	}

	// handle template defaults
	if s.Template == nil {
		s.Template = &TemplateConfig{}
	}
	if s.Template.OK == "" {
		s.Template.OK = defaultTemplateOK
	}
	if s.Template.ALERT == "" {
		s.Template.ALERT = defaultTemplateALERT
	}
	// parse templates
	var err error
	if s.Template.tOK, err = template.New("OK").Parse(s.Template.OK); err != nil {
		return err
	}
	if s.Template.tALERT, err = template.New("ALERT").Parse(s.Template.ALERT); err != nil {
		return err
	}

	// initialize monitors
	for _, monitor := range s.Monitors {
		monitor.Init()
	}

	// create http client
	ctx := context.Background()
	client, err := s.createClient(ctx)
	if err != nil {
		return err
	}
	s.client = client

	return nil
}

// Close any connections to the Output
func (s *Skyline) Close() error {
	return nil
}

// Description returns a one-sentence description on the Output
func (s *Skyline) Description() string {
	return "A plugin that send access log alerts over HTTP"
}

// SampleConfig returns the default configuration of the Output
func (s *Skyline) SampleConfig() string {
	return sampleConfig
}

// Write takes in group of points to be written to the Output
func (s *Skyline) Write(metrics []telegraf.Metric) error {
	for _, monitor := range s.Monitors {
		for _, metric := range metrics {
			monitor.ProcessMetric(metric)
		}
		for _, alert := range monitor.ShowAlerts(s.Template) {
			go s.write([]byte(alert))
		}
	}

	return nil
}

func (s *Skyline) write(reqBody []byte) error {
	var reqBodyBuffer io.Reader = bytes.NewBuffer(reqBody)

	var err error
	req, err := http.NewRequest("POST", s.URL, reqBodyBuffer)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", "Telegraf/"+internal.Version())
	req.Header.Set("Content-Type", defaultContentType)

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = ioutil.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("when writing to [%s] received status code: %d", s.URL, resp.StatusCode)
	}

	return nil
}

func init() {
	outputs.Add("skyline", func() telegraf.Output {
		return &Skyline{
			Timeout: internal.Duration{Duration: defaultClientTimeout},
		}
	})
}
