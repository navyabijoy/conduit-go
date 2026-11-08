package main

import "testing"

func TestCheckWebhookSecret(t *testing.T) {
	tests := []struct {
		name       string
		secret     string
		production bool
		wantErr    bool
	}{
		{"dev default allowed outside production", devWebhookSecret, false, false},
		{"dev default refused in production", devWebhookSecret, true, true},
		{"empty refused outside production", "", false, true},
		{"empty refused in production", "", true, true},
		{"own secret allowed in production", "s3cret-from-ops", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkWebhookSecret(tt.secret, tt.production)
			if (err != nil) != tt.wantErr {
				t.Fatalf("checkWebhookSecret(%q, %v) = %v, wantErr %v", tt.secret, tt.production, err, tt.wantErr)
			}
		})
	}
}
