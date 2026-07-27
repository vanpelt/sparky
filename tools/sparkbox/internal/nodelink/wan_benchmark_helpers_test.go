package nodelink

import (
	"fmt"
	"strings"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

type wanTransportTotals struct {
	DisconnectsTotal float64 `json:"disconnects_total"`
	DroppedTotal     float64 `json:"dropped_total"`
	WriteQueueDepth  float64 `json:"write_queue_depth"`
}

func parseWANTransportTotals(scrape, node, transport string) (wanTransportTotals, error) {
	parser := expfmt.NewTextParser(model.LegacyValidation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(scrape))
	if err != nil {
		return wanTransportTotals{}, fmt.Errorf("parse fleet metrics: %w", err)
	}
	var totals wanTransportTotals
	totals.DisconnectsTotal, err = sumWANMetric(
		families["sparkbox_fleet_disconnects_total"], node, transport, true,
	)
	if err != nil {
		return wanTransportTotals{}, err
	}
	totals.DroppedTotal, err = sumWANMetric(
		families["sparkbox_fleet_control_dropped_total"], node, transport, true,
	)
	if err != nil {
		return wanTransportTotals{}, err
	}
	totals.WriteQueueDepth, err = sumWANMetric(
		families["sparkbox_fleet_control_write_queue_depth"], node, transport, false,
	)
	if err != nil {
		return wanTransportTotals{}, err
	}
	return totals, nil
}

func sumWANMetric(family *dto.MetricFamily, node, transport string, counter bool) (float64, error) {
	if family == nil {
		return 0, nil
	}
	var total float64
	for _, metric := range family.GetMetric() {
		if wanMetricLabel(metric, "node") != node ||
			wanMetricLabel(metric, "transport") != transport {
			continue
		}
		if counter {
			if metric.GetCounter() == nil {
				return 0, fmt.Errorf("%s is not a counter", family.GetName())
			}
			total += metric.GetCounter().GetValue()
		} else {
			if metric.GetGauge() == nil {
				return 0, fmt.Errorf("%s is not a gauge", family.GetName())
			}
			total += metric.GetGauge().GetValue()
		}
	}
	return total, nil
}

func wanMetricLabel(metric *dto.Metric, name string) string {
	for _, label := range metric.GetLabel() {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
}
