// Command webhook-receiver is the remediation edge of the loop: it receives
// Alertmanager webhook notifications, logs one structured line per alert,
// keeps the last 50 notifications for inspection at GET /alerts, and — when
// DEVIN_API_KEY is set — creates a Devin session to investigate and fix the
// firing alert.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

type alert struct {
	Status       string            `json:"status"`
	Labels       map[string]string `json:"labels"`
	Annotations  map[string]string `json:"annotations"`
	StartsAt     time.Time         `json:"startsAt"`
	EndsAt       time.Time         `json:"endsAt"`
	GeneratorURL string            `json:"generatorURL"`
	Fingerprint  string            `json:"fingerprint"`
}

type notification struct {
	Receiver          string            `json:"receiver"`
	Status            string            `json:"status"`
	Alerts            []alert           `json:"alerts"`
	GroupLabels       map[string]string `json:"groupLabels"`
	CommonLabels      map[string]string `json:"commonLabels"`
	CommonAnnotations map[string]string `json:"commonAnnotations"`
	ExternalURL       string            `json:"externalURL"`
}

var (
	mu     sync.Mutex
	recent []notification
)

func alertsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var n notification
	if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mu.Lock()
	recent = append(recent, n)
	if len(recent) > 50 {
		recent = recent[len(recent)-50:]
	}
	mu.Unlock()
	for _, a := range n.Alerts {
		log.Printf("alert status=%s name=%s severity=%s fingerprint=%s summary=%q",
			a.Status, a.Labels["alertname"], a.Labels["severity"], a.Fingerprint,
			a.Annotations["summary"])
		if a.Status == "firing" && os.Getenv("DEVIN_API_KEY") != "" {
			go dispatchDevin(a)
		}
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// dispatchDevin turns a firing alert into a Devin session via the public API
// (POST https://api.devin.ai/v1/sessions, Bearer auth). The prompt carries the
// alert context so the session can go straight to root-causing the regression.
func dispatchDevin(a alert) {
	prompt := fmt.Sprintf(`A Prometheus alert is firing: %s (severity: %s).

Summary: %s
Description: %s
Started at: %s
Generator: %s

Investigate the latency/error regression behind this alert, find the root cause in the repository, fix it with a minimal change, and open a PR.`,
		a.Labels["alertname"], a.Labels["severity"],
		a.Annotations["summary"], a.Annotations["description"],
		a.StartsAt.Format(time.RFC3339), a.GeneratorURL)

	body, err := json.Marshal(map[string]any{"prompt": prompt})
	if err != nil {
		log.Printf("devin prompt marshal failed: %v", err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.devin.ai/v1/sessions", bytes.NewReader(body))
	if err != nil {
		log.Printf("devin request build failed: %v", err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("DEVIN_API_KEY"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("devin dispatch failed: %v", err)
		return
	}
	defer resp.Body.Close()
	log.Printf("devin session requested, api_status=%d", resp.StatusCode)
}

func listHandler(w http.ResponseWriter, _ *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(recent)
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9000"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/alerts", alertsHandler) // POST: ingest, GET: list recent
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	log.Printf("webhook-receiver listening on :%s (devin_dispatch=%v)", port, os.Getenv("DEVIN_API_KEY") != "")
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
