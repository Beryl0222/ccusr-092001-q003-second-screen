package main

import "testing"

func TestHealthIdentity(t *testing.T) {
	if health()["service"] != serviceID {
		t.Fatal("服务身份不一致")
	}
}
