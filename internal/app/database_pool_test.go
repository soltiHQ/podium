package app

import (
	"strings"
	"testing"
	"time"
)

// A pool's driver configuration takes the URL's host, port, database and
// credentials, names the pool by its application_name, which replaces the URL's
// own, and gets a connect timeout of 5 s unless the URL sets one other than 0,
// which it keeps. A URL that does not parse fails with an error that names the
// setting and carries nothing of the URL. It needs no Postgres: nothing
// connects.
func TestPoolConfig(t *testing.T) {
	const secretURL = "postgres://pool_cfg_user:pool-cfg-pw-31b7@db.podium.test:6432/podium?application_name=theirs"
	cfg, err := poolConfig("cluster.database.url", secretURL, replicaName)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "db.podium.test" || cfg.Port != 6432 || cfg.Database != "podium" || cfg.User != "pool_cfg_user" || cfg.Password != "pool-cfg-pw-31b7" {
		t.Errorf("the URL parsed to %s:%d/%s as %s, want db.podium.test:6432/podium as pool_cfg_user with its password", cfg.Host, cfg.Port, cfg.Database, cfg.User)
	}
	if got := cfg.RuntimeParams["application_name"]; got != replicaName {
		t.Errorf("application_name %q, want %q in place of the URL's", got, replicaName)
	}
	for _, tc := range []struct {
		query string
		want  time.Duration
	}{
		{"", connectTimeout},
		{"connect_timeout=0", connectTimeout},
		{"connect_timeout=2", 2 * time.Second},
	} {
		cfg, err := poolConfig("cluster.database.url", "postgres://u@db.podium.test/podium?"+tc.query, primaryName)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ConnectTimeout != tc.want {
			t.Errorf("%q: connect timeout %s, want %s", tc.query, cfg.ConnectTimeout, tc.want)
		}
	}
	if connectTimeout != 5*time.Second {
		t.Errorf("the connect timeout of a URL without one is %s, want 5s", connectTimeout)
	}

	malformed := "postgres://pool_cfg_user:pool-cfg-pw-31b7@db.podium.test:notaport/podium"
	_, err = poolConfig("cluster.database.replica_url", malformed, replicaName)
	if err == nil || !strings.Contains(err.Error(), "cluster.database.replica_url") {
		t.Fatalf("a malformed URL: %v, want an error naming cluster.database.replica_url", err)
	}
	for _, secret := range []string{"pool-cfg-pw-31b7", "pool_cfg_user", "db.podium.test", "postgres://"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error of a malformed URL carries %q", secret)
		}
	}
}
