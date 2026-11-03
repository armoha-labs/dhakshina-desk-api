package config

import "testing"

func TestMongoDBFollowsEnv(t *testing.T) {
	for _, tc := range []struct{ env, override, want string }{
		{"", "", "dhakshinadeskdev"},
		{"development", "", "dhakshinadeskdev"},
		{"production", "", "dhakshina_desk"},
		{"development", "other", "other"},
	} {
		t.Setenv("APP_ENV", tc.env)
		t.Setenv("MONGODB_DB", tc.override)
		t.Setenv("MONGODB_URI", "mongodb://localhost")
		for _, s := range []string{"JWT_SECRET", "LINK_SECRET", "FIELD_ENC_KEY", "ADMIN_TOKEN_SECRET"} {
			t.Setenv(s, "test")
		}
		t.Setenv("PAYMENT_GATEWAY", "razorpay")
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.MongoDB != tc.want {
			t.Errorf("APP_ENV=%q MONGODB_DB=%q: got %q, want %q", tc.env, tc.override, c.MongoDB, tc.want)
		}
	}
}
