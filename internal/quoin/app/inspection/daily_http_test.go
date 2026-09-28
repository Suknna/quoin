package appinspection

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDailyReportConfigAndBackfillOverHTTP(t *testing.T) {
	_, db, api := newPlanScopeHarness(t)
	for _, statement := range []string{
		`INSERT INTO users(id,username,display_name,role,enabled,initialized,password_phc,row_version,created_at,updated_at) VALUES(1,'admin','Admin','admin',1,1,'fixture',1,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`,
		`INSERT INTO sessions(id,user_id,session_token_digest,auth_revision_at_issue,client_label,created_at,last_active_at,idle_expires_at,absolute_expires_at) VALUES(1,1,zeroblob(32),1,'daily-http','2026-09-01T00:00:00Z','2026-09-01T00:00:00Z','2099-01-01T00:00:00Z','2099-01-01T00:00:00Z')`,
		`INSERT INTO connections(id,name,type,enabled,row_version,created_at) VALUES(1,'daily-source','prometheus',1,1,'2026-09-01T00:00:00Z')`,
		`INSERT INTO inspection_plans(plan_key,display_name,enabled,connection_id,plugin_id,template_id,params_json,scope_kind,scope_json,timezone,row_version,created_at,updated_at) VALUES('daily-plan','Daily plan',1,1,'prometheus','promql_instant','{"expression":"up"}','integration','{"kind":"integration"}','UTC',1,'2026-09-01T00:00:00Z','2026-09-01T00:00:00Z')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	headers := map[string][]string{"Cookie": {"__Host-quoin-session=test"}, "Content-Type": {"application/json"}}
	response := api.Post("/api/v1/inspections/daily-report-configs", strings.NewReader(`{
		"clientCommandId":"create-daily-http-1","configKey":"daily-http","displayName":"Daily checks",
		"enabled":true,"timezone":"Asia/Shanghai","triggerTime":"09:00","planKeys":["daily-plan"],
		"reportInstructions":"按来源说明缺口并引用 Evidence 编号"
	}`), headers)
	if response.Code != http.StatusCreated {
		t.Fatalf("create daily config status=%d body=%s", response.Code, response.Body.String())
	}
	var config struct {
		ConfigKey          string `json:"configKey"`
		Timezone           string `json:"timezone"`
		TriggerTime        string `json:"triggerTime"`
		ReportInstructions string `json:"reportInstructions"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &config); err != nil || config.ConfigKey != "daily-http" || config.Timezone != "Asia/Shanghai" || config.TriggerTime != "09:00" || config.ReportInstructions != "按来源说明缺口并引用 Evidence 编号" {
		t.Fatalf("config=%+v err=%v", config, err)
	}
	response = api.Get("/api/v1/inspections/daily-report-configs/daily-http", headers)
	if response.Code != http.StatusOK {
		t.Fatalf("read daily config status=%d body=%s", response.Code, response.Body.String())
	}
	response = api.Get("/api/v1/inspections/daily-reports/missing?configKey=daily-http", headers)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"items":[]`) {
		t.Fatalf("missing-day read status=%d body=%s", response.Code, response.Body.String())
	}
	response = api.Get("/api/v1/inspections/daily-reports/missing?configKey=unknown", headers)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown config missing-day status=%d body=%s", response.Code, response.Body.String())
	}
	response = api.Post("/api/v1/inspections/daily-reports/backfill", strings.NewReader(`{
		"clientCommandId":"backfill-daily-http-1","configKey":"daily-http","localDate":"2026-09-26"
	}`), headers)
	if response.Code != http.StatusAccepted {
		t.Fatalf("backfill daily status=%d body=%s", response.Code, response.Body.String())
	}
	response = api.Get("/api/v1/inspections/daily-reports/daily-http/2026-09-26", headers)
	if response.Code != http.StatusOK {
		t.Fatalf("read daily status=%d body=%s", response.Code, response.Body.String())
	}
	var detail struct {
		LocalDate      string `json:"localDate"`
		Timezone       string `json:"timezone"`
		WindowStartUTC string `json:"windowStartUtc"`
		WindowEndUTC   string `json:"windowEndUtc"`
		State          string `json:"state"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &detail); err != nil || detail.LocalDate != "2026-09-26" || detail.Timezone != "Asia/Shanghai" || detail.WindowStartUTC != "2026-09-25T16:00:00Z" || detail.WindowEndUTC != "2026-09-26T16:00:00Z" || detail.State != "Collecting" {
		t.Fatalf("frozen daily report=%+v err=%v", detail, err)
	}
	response = api.Get("/api/v1/inspections/daily-reports/daily-http/2026-09-26/analyses", headers)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"items":[]`) {
		t.Fatalf("analysis before fact sealing status=%d body=%s", response.Code, response.Body.String())
	}
	response = api.Get("/api/v1/inspections/daily-reports/daily-http/2026-09-26/analyses/1", headers)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown analysis status=%d body=%s", response.Code, response.Body.String())
	}
}
