package basicstats

import (
	"log"
	"regexp"
	"strconv"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/plugins/aggregators"
	"github.com/influxdata/telegraf/plugins/inputs/statsd"
)

type BasicStats struct {
	Stats  []string            `toml:"stats"`
	Fields map[string][]string `toml:"fields"`

	cache   map[uint64]aggregate
	configs map[string]configuredStats
}

type configuredStats struct {
	count       bool
	min         bool
	max         bool
	mean        bool
	variance    bool
	stdev       bool
	sum         bool
	percentiles []int
}

func NewBasicStats() *BasicStats {
	mm := &BasicStats{}
	mm.Reset()
	return mm
}

type aggregate struct {
	fields map[string]statsd.RunningStats
	name   string
	tags   map[string]string
}

var sampleConfig = `
  ## The period on which to flush & clear the aggregator.
  period = "30s"
  ## If true, the original metric will be dropped by the
  ## aggregator and will not get sent to the output plugins.
  drop_original = false

  ## Configures which basic stats to push as fields. This option
  ## is deprecated and only kept for backward compatibility. If any
  ## fields is configured, this option will be ignored.
  # stats = ["count", "min", "max", "mean", "stdev", "s2", "sum"]

  ## Configures which basic stats to push as fields. "*" is the default configuration for all fields.
  ## Use strings like "p95" to add 95th percentile. Supported percentile range is [0, 100].
  # [aggregators.basicstats.fields]
  #   "*" = ["count", "min", "max", "mean", "stdev", "s2", "sum"]
  #   "some_field" = ["count", "p90", "p95"]
  ## If "*" is not provided, unmatched fields will be ignored.
  # [aggregators.basicstats.fields]
  #   "only_field" = ["count", "sum"]
`

func (m *BasicStats) SampleConfig() string {
	return sampleConfig
}

func (m *BasicStats) Description() string {
	return "Keep the aggregate statsd.RunningStats of each metric passing through."
}

func (m *BasicStats) Add(in telegraf.Metric) {
	id := in.HashID()
	if _, ok := m.cache[id]; !ok {
		// hit an uncached metric, create caches for first time:
		a := aggregate{
			name:   in.Name(),
			tags:   in.Tags(),
			fields: make(map[string]statsd.RunningStats),
		}
		for _, field := range in.FieldList() {
			if fv, ok := convert(field.Value); ok {
				rs := statsd.RunningStats{}
				rs.AddValue(fv)
				a.fields[field.Key] = rs
			}
		}
		m.cache[id] = a
	} else {
		for _, field := range in.FieldList() {
			if fv, ok := convert(field.Value); ok {
				if _, ok := m.cache[id].fields[field.Key]; !ok {
					// hit an uncached field of a cached metric
					m.cache[id].fields[field.Key] = statsd.RunningStats{}
				}

				rs := m.cache[id].fields[field.Key]
				rs.AddValue(fv)
				m.cache[id].fields[field.Key] = rs
			}
		}
	}
}

func (m *BasicStats) Push(acc telegraf.Accumulator) {
	for _, aggregate := range m.cache {
		fields := map[string]interface{}{}
		for k, v := range aggregate.fields {
			config := m.getConfiguredStatsForField(k)

			if config.count {
				fields[k+"_count"] = v.Count()
			}
			if config.min {
				fields[k+"_min"] = v.Lower()
			}
			if config.max {
				fields[k+"_max"] = v.Upper()
			}
			if config.mean {
				fields[k+"_mean"] = v.Mean()
			}
			if config.sum {
				fields[k+"_sum"] = v.Sum()
			}

			for _, p := range config.percentiles {
				fields[k+"_p"+strconv.Itoa(p)] = v.Percentile(p)
			}

			// backward compatibility
			if v.Count() > 1 {
				if config.variance {
					fields[k+"_s2"] = v.Variance()
				}
				if config.stdev {
					fields[k+"_stdev"] = v.Stddev()
				}
			}
		}

		if len(fields) > 0 {
			acc.AddFields(aggregate.name, fields, aggregate.tags)
		}
	}
}

func parseStats(stats []string) configuredStats {

	PRECENTILE_PATTERN := regexp.MustCompile(`^p([0-9]|[1-9][0-9]|100)$`)

	parsed := configuredStats{}

	for _, stat := range stats {

		// parse percentile stats, e.g. "p90" "p95"
		match := PRECENTILE_PATTERN.FindStringSubmatch(stat)
		if len(match) >= 2 {
			if p, err := strconv.Atoi(match[1]); err == nil {
				parsed.percentiles = append(parsed.percentiles, p)
				continue
			}
		}

		switch stat {

		case "count":
			parsed.count = true
		case "min":
			parsed.min = true
		case "max":
			parsed.max = true
		case "mean":
			parsed.mean = true
		case "s2":
			parsed.variance = true
		case "stdev":
			parsed.stdev = true
		case "sum":
			parsed.sum = true

		default:
			log.Printf("W! Unrecognized basic stat '%s', ignoring", stat)
		}
	}

	return parsed
}

func (m *BasicStats) getConfiguredStatsForField(field string) configuredStats {
	DEFAULT_FIELD := "*"
	DEFAULT_STATS := []string{"count", "min", "max", "mean", "s2", "stdev"}

	if m.configs == nil {

		if m.Fields == nil {
			m.Fields = make(map[string][]string)

			if m.Stats == nil {
				// neither m.Fileds nor m.Stats provided, use DEFAULT_STATS
				m.Fields[DEFAULT_FIELD] = DEFAULT_STATS
			} else {
				// make m.Stats default for all fields
				m.Fields[DEFAULT_FIELD] = m.Stats
			}
		}
		// m.Fields provided, m.Stats ignored

		m.configs = make(map[string]configuredStats)

		for k, stats := range m.Fields {
			m.configs[k] = parseStats(stats)
		}
	}

	if _, ok := m.configs[field]; !ok {
		// field-specfic stats not found, fallback to DEFAULT_FIELD
		field = DEFAULT_FIELD
	}
	// it's OK if DEFAULT_FIELD is not specified, the return below won't
	// result in any error or aggregated field, which is what we desired

	return m.configs[field]
}

func (m *BasicStats) Reset() {
	m.cache = make(map[uint64]aggregate)
}

func convert(in interface{}) (float64, bool) {
	switch v := in.(type) {
	case float64:
		return v, true
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	default:
		return 0, false
	}
}

func init() {
	aggregators.Add("basicstats", func() telegraf.Aggregator {
		return NewBasicStats()
	})
}
