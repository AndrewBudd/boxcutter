package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCheckServiceHealth_SuccessResetsCounter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	sh := &serviceHealth{healthy: true}
	client := srv.Client()

	checkServiceHealth(client, "test", srv.URL, sh)

	if !sh.healthy {
		t.Error("should be healthy after successful check")
	}
	if sh.consecutiveFails != 0 {
		t.Errorf("consecutiveFails = %d, want 0", sh.consecutiveFails)
	}
	if sh.lastHealthy.IsZero() {
		t.Error("lastHealthy should be set")
	}
}

func TestCheckServiceHealth_ThresholdBehavior(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	sh := &serviceHealth{healthy: true}
	client := srv.Client()

	// 1 fail: still healthy
	checkServiceHealth(client, "test", srv.URL, sh)
	if !sh.healthy {
		t.Error("should still be healthy after 1 fail")
	}

	// 2 fails: still healthy
	checkServiceHealth(client, "test", srv.URL, sh)
	if !sh.healthy {
		t.Error("should still be healthy after 2 fails")
	}

	// 3 fails: unhealthy (threshold = 3)
	checkServiceHealth(client, "test", srv.URL, sh)
	if sh.healthy {
		t.Error("should be unhealthy after 3 consecutive fails")
	}
	if sh.consecutiveFails != 3 {
		t.Errorf("consecutiveFails = %d, want 3", sh.consecutiveFails)
	}
}

func TestCheckServiceHealth_RecoveryAfterDown(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if callCount <= 3 {
			w.WriteHeader(500)
		} else {
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()

	sh := &serviceHealth{healthy: true}
	client := srv.Client()

	// 3 fails → unhealthy
	for i := 0; i < 3; i++ {
		checkServiceHealth(client, "test", srv.URL, sh)
	}
	if sh.healthy {
		t.Fatal("should be unhealthy after 3 fails")
	}

	// 1 success → recovery
	checkServiceHealth(client, "test", srv.URL, sh)
	if !sh.healthy {
		t.Error("should recover after success")
	}
	if sh.consecutiveFails != 0 {
		t.Errorf("consecutiveFails = %d, want 0 after recovery", sh.consecutiveFails)
	}
}

func TestCheckServiceHealth_ConnectionError(t *testing.T) {
	// Server that doesn't exist → connection error
	sh := &serviceHealth{healthy: true}
	client := &http.Client{Timeout: 100 * time.Millisecond}

	checkServiceHealth(client, "test", "http://127.0.0.1:1", sh)
	if sh.consecutiveFails != 1 {
		t.Errorf("consecutiveFails = %d, want 1 after connection error", sh.consecutiveFails)
	}
	if !sh.healthy {
		t.Error("should still be healthy after 1 connection error")
	}
}

func TestCheckServiceHealth_LastCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	sh := &serviceHealth{}
	client := srv.Client()

	before := time.Now()
	checkServiceHealth(client, "test", srv.URL, sh)

	if sh.lastCheck.Before(before) {
		t.Error("lastCheck should be updated")
	}
}
