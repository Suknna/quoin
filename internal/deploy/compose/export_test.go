package compose

// Test-only exports of the pure verifier judges so the external test package
// can exercise them against real catalog projections without re-implementing
// exposition rendering.
var JudgeMetricsForTest = judgeMetrics

var JudgeLogsForTest = judgeLogs
