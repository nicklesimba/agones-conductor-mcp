package main

import (
	"testing"
	"time"
)

func TestAge_AllThreeBranches(t *testing.T) {
	cases := []struct {
		name string
		ago  time.Duration
		want string
	}{
		{"future timestamp clamps to zero", -5 * time.Minute, "0m"},
		{"minutes", 5 * time.Minute, "5m"},
		{"just under an hour", 59 * time.Minute, "59m"},
		{"hours and minutes", 2*time.Hour + 15*time.Minute, "2h15m"},
		{"just under a day", 23 * time.Hour, "23h0m"},
		{"days", 3 * 24 * time.Hour, "3d"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := age(time.Now().Add(-c.ago))
			if got != c.want {
				t.Errorf("age(%v ago) = %q, want %q", c.ago, got, c.want)
			}
		})
	}
}
