package server

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync"

	"splash/goserver/ojson"
)

type lineWriter struct {
	mu sync.Mutex
	f  *os.File
}

func (w *lineWriter) write(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, _ = w.f.WriteString(s)
}

var stdout = &lineWriter{f: os.Stdout}
var stderr = &lineWriter{f: os.Stderr}

func labelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func metricNumber(v any) (string, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return "1", true
		}
		return "0", true
	case int64:
		return fmt.Sprint(x), true
	case float64:
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return "", false
		}
		return ojson.FloatRepr(x), true
	}
	return "", false
}

func pathValue(o *ojson.Object, path []string) any {
	var cur any = o
	for _, k := range path {
		obj, ok := cur.(*ojson.Object)
		if !ok || !obj.Has(k) {
			return nil
		}
		cur = obj.Value(k)
	}
	return cur
}

// prometheusMetrics renders the status as metrics.prometheus_metrics does.
func prometheusMetrics(status *ojson.Object) string {
	lines := []string{"# HELP splash_info Splash runtime metrics.", "# TYPE splash_info gauge", `splash_info{runtime="native"} 1`}
	state := func(name string, value any, states ...string) {
		for _, s := range states {
			v := 0
			if value == s {
				v = 1
			}
			lines = append(lines, fmt.Sprintf(`%s{state="%s"} %d`, name, s, v))
		}
	}
	state("splash_memory_pressure", status.Value("memory_pressure"), "normal", "warning", "critical")
	state("splash_thermal_state", status.Value("thermal_state"), "nominal", "fair", "serious", "critical")
	state("splash_disk_writes_gated", pathValue(status, []string{"disk", "writes_gated"}), "no", "budget", "closed")
	if accounts, ok := pathValue(status, []string{"memory_governor", "accounts"}).([]any); ok {
		for _, a := range accounts {
			acc, ok := a.(*ojson.Object)
			if !ok {
				continue
			}
			name, _ := acc.Value("name").(string)
			label := fmt.Sprintf(`account="%s"`, labelValue(name))
			for _, m := range []string{"floor", "current", "cap"} {
				n, _ := metricNumber(acc.Value(m + "_bytes"))
				lines = append(lines, fmt.Sprintf(`splash_memory_account_bytes{%s,measure="%s"} %s`, label, m, n))
			}
			g, _ := metricNumber(acc.Value("grows"))
			s, _ := metricNumber(acc.Value("shrinks"))
			lines = append(lines, fmt.Sprintf("splash_memory_account_grows_total{%s} %s", label, g))
			lines = append(lines, fmt.Sprintf("splash_memory_account_shrinks_total{%s} %s", label, s))
		}
	}
	if widths, ok := pathValue(status, []string{"scheduler", "decode_batches_by_width"}).(*ojson.Object); ok {
		for _, w := range widths.Keys() {
			if _, isBool := widths.Value(w).(bool); isBool {
				continue
			}
			if n, ok := metricNumber(widths.Value(w)); ok {
				lines = append(lines, fmt.Sprintf("splash_scheduler_decode_%s_total %s", w, n))
			}
		}
	}
	for _, s := range prometheusSeries {
		if n, ok := metricNumber(pathValue(status, s.path)); ok {
			lines = append(lines, s.name+" "+n)
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
