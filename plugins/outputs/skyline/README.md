# Skyline Output Plugin

This plugin sends access log alerts over HTTP.

### Configuration:

```toml
# A plugin that send access log alerts over HTTP
[[outputs.skyline]]
  ## URL is the address to send alerts to
  url = "http://127.0.0.1:8080/alert"

  ## Timeout for HTTP message
  # timeout = "5s"

  ## Alert message template
  # [outputs.skyline.template]
  #   OK = "[{{ .Now }}] OK: {{ .Monitor.Name }} {{ .Alert.Formula }}"
  #   ALERT = "[{{ .Now }}] WARN: {{ .Monitor.Name }} {{ .Alert.Formula }}"

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
```
