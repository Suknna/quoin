package inspection

// 日报测量投影测试：逐序列有界摘要（标签 + 数值极值 + 计数 + 截断标记），
// 中间序列的异常值与标签必须可分析；recover 边界必须把 panic 变成确定性
// 类型错误，绝不返回 (nil, nil) 的伪成功。

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Suknna/quoin/internal/agentcontext"
)

func TestDailyMeasurementFromEvidenceKeepsMiddleSeriesAnomalyAndLabels(t *testing.T) {
	result := `{"resultType":"vector","result":[
		{"metric":{"instance":"a","job":"quoin"},"value":[1790000000,"0.4"]},
		{"metric":{"instance":"b","job":"quoin"},"value":[1790000001,"9.9"]},
		{"metric":{"instance":"c"},"value":[1790000002,"0.5"]},
		{"metric":{"instance":"d"},"value":[1790000003,"NaN"]}]}`
	measurement, err := dailyMeasurementFromEvidence(11, "latency", result)
	if err != nil {
		t.Fatalf("projection failed: %v", err)
	}
	if measurement.Series != 4 || measurement.Samples != 4 {
		t.Fatalf("counts = %+v", measurement)
	}
	// 跨序列首末值保持既有语义；NaN 只计数，不参与数值极值。
	if *measurement.FirstValue != "0.4" || *measurement.LastValue != "NaN" || *measurement.LastAt != "1790000003" {
		t.Fatalf("cross-series summary = %+v", measurement)
	}
	if len(measurement.Entries) != 4 || measurement.Truncated {
		t.Fatalf("entries = %+v", measurement.Entries)
	}
	// 中间序列的异常尖峰与识别它的标签必须可见。
	spike := measurement.Entries[1]
	if !reflect.DeepEqual(spike.Labels, map[string]string{"instance": "b", "job": "quoin"}) {
		t.Fatalf("spike labels = %+v", spike.Labels)
	}
	if *spike.MaxValue != "9.9" || *spike.MaxAt != "1790000001" {
		t.Fatalf("spike extremes = %+v", spike)
	}
	if *spike.FirstValue != "9.9" || *spike.LastValue != "9.9" || spike.Samples != 1 {
		t.Fatalf("spike summary = %+v", spike)
	}
	// NaN 不产生 min/max；普通序列的极值与时间戳成对出现。
	for index, want := range map[int][2]string{0: {"0.4", "0.4"}, 2: {"0.5", "0.5"}} {
		entry := measurement.Entries[index]
		if *entry.MinValue != want[0] || *entry.MaxValue != want[1] {
			t.Fatalf("entry %d extremes = %+v", index, entry)
		}
		if entry.MinAt == nil || entry.MaxAt == nil {
			t.Fatalf("entry %d must timestamp its extremes", index)
		}
	}
	if measurement.Entries[3].MinValue != nil || measurement.Entries[3].MaxValue != nil {
		t.Fatalf("NaN must not join numeric extremes: %+v", measurement.Entries[3])
	}
	if measurement.Entries[3].Samples != 1 || *measurement.Entries[3].LastValue != "NaN" {
		t.Fatalf("NaN sample must still be counted honestly: %+v", measurement.Entries[3])
	}
}

func TestDailyMeasurementFromEvidenceMatrixDipAndScalarShape(t *testing.T) {
	matrix := `{"resultType":"matrix","result":[
		{"metric":{"mode":"idle"},"values":[[1790000000,"90"],[1790000001,"3.5"],[1790000002,"88"]]},
		{"metric":{"mode":"user"},"values":[[1790000000,"10"],[1790000001,"12"],[1790000002,"11"]]}]}`
	measurement, err := dailyMeasurementFromEvidence(12, "cpu", matrix)
	if err != nil {
		t.Fatalf("projection failed: %v", err)
	}
	if measurement.Series != 2 || measurement.Samples != 6 {
		t.Fatalf("counts = %+v", measurement)
	}
	idle := measurement.Entries[0]
	if *idle.MinValue != "3.5" || *idle.MinAt != "1790000001" || *idle.MaxValue != "90" || *idle.MaxAt != "1790000000" {
		t.Fatalf("dip extremes = %+v", idle)
	}
	// 首末值都看不到 03:00 型的中间凹陷：极值正是为此存在。
	if *idle.FirstValue != "90" || *idle.LastValue != "88" {
		t.Fatalf("idle boundary values = %+v", idle)
	}
	scalar := `{"resultType":"scalar","result":[1790000009,"42"]}`
	scalarMeasurement, err := dailyMeasurementFromEvidence(13, "answer", scalar)
	if err != nil {
		t.Fatalf("scalar projection failed: %v", err)
	}
	if scalarMeasurement.Series != 1 || scalarMeasurement.Samples != 1 || len(scalarMeasurement.Entries) != 1 {
		t.Fatalf("scalar summary = %+v", scalarMeasurement)
	}
	if *scalarMeasurement.Entries[0].FirstValue != "42" || *scalarMeasurement.LastValue != "42" {
		t.Fatalf("scalar value must not be lost: %+v", scalarMeasurement.Entries[0])
	}
}

func TestDailyMeasurementFromEvidenceBoundsSeriesAndLabels(t *testing.T) {
	series := []string{}
	for index := 0; index < agentcontext.DailyMeasurementEntryBound+8; index++ {
		series = append(series, `{"metric":{"instance":"n`+strings.Repeat("x", index)+`"},"value":[1790000000,"1"]}`)
	}
	result := `{"resultType":"vector","result":[` + strings.Join(series, ",") + `]}`
	measurement, err := dailyMeasurementFromEvidence(14, "fanout", result)
	if err != nil {
		t.Fatalf("projection failed: %v", err)
	}
	// 超界序列按确定性顺序省略并诚实标记，计数保持全量。
	if measurement.Series != agentcontext.DailyMeasurementEntryBound+8 || measurement.Samples != agentcontext.DailyMeasurementEntryBound+8 {
		t.Fatalf("counts = %+v", measurement)
	}
	if len(measurement.Entries) != agentcontext.DailyMeasurementEntryBound || !measurement.Truncated {
		t.Fatalf("entries = %d truncated = %v", len(measurement.Entries), measurement.Truncated)
	}
	labels := map[string]string{}
	for index := 0; index < agentcontext.DailyMeasurementLabelBound+5; index++ {
		labels[strings.Repeat("k", index+1)] = "v"
	}
	body, err := json.Marshal(map[string]any{
		"resultType": "vector",
		"result":     []map[string]any{{"metric": labels, "value": []string{"1790000000", "2"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	labelMeasurement, err := dailyMeasurementFromEvidence(15, "labels", string(body))
	if err != nil {
		t.Fatalf("label projection failed: %v", err)
	}
	entry := labelMeasurement.Entries[0]
	if len(entry.Labels) != agentcontext.DailyMeasurementLabelBound || !entry.LabelsTruncated {
		t.Fatalf("labels = %d truncated = %v", len(entry.Labels), entry.LabelsTruncated)
	}
	// 确定性：同一输入重复投影得到逐字节相同摘要（标签映射序列化稳定）。
	replay, err := dailyMeasurementFromEvidence(15, "labels", string(body))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(labelMeasurement)
	second, _ := json.Marshal(replay)
	if string(first) != string(second) {
		t.Fatal("measurement projection must be deterministic")
	}
}

func TestDailyMeasurementFromEvidenceRejectsMalformedResult(t *testing.T) {
	for name, body := range map[string]string{
		"unparseable":   `{not-json`,
		"broken vector": `{"resultType":"vector","result":{"not":"an array"}}`,
		"broken matrix": `{"resultType":"matrix","result":"x"}`,
		"broken scalar": `{"resultType":"scalar","result":"lonely"}`,
	} {
		measurement, err := dailyMeasurementFromEvidence(16, "check", body)
		if err == nil || measurement != nil {
			t.Fatalf("%s: (measurement, err) = (%+v, %v), want (nil, typed error)", name, measurement, err)
		}
		if !strings.Contains(err.Error(), "evidence 16") || !strings.Contains(err.Error(), "check") {
			t.Fatalf("%s: error must carry identity: %v", name, err)
		}
	}
}

// TestDailyMeasurementRecoverBoundaryReturnsTypedError 钉住 recover 边界的
// 真实语义：投影内部的 panic 必须变成 (nil, 类型化错误)，绝不能是未命名
// 返回值时代那份静默的 (nil, nil) 伪成功。
func TestDailyMeasurementRecoverBoundaryReturnsTypedError(t *testing.T) {
	original := dailyMeasurementProject
	defer func() { dailyMeasurementProject = original }()
	dailyMeasurementProject = func(int64, string, string) (*agentcontext.DailyMeasurement, error) {
		panic("projection exploded")
	}
	measurement, err := dailyMeasurementFromEvidence(17, "latency", `{"resultType":"vector","result":[]}`)
	if measurement != nil {
		t.Fatalf("recovered projection must not return a measurement: %+v", measurement)
	}
	if err == nil {
		t.Fatal("recovered projection must surface a typed error, not a silent (nil, nil) success")
	}
	if !strings.Contains(err.Error(), "evidence 17") || !strings.Contains(err.Error(), "latency") {
		t.Fatalf("error must carry identity: %v", err)
	}
	if !strings.Contains(err.Error(), "measurement projection failed") {
		t.Fatalf("error must name the projection failure: %v", err)
	}
}
