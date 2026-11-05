package discovery

import (
	"fmt"

	"github.com/soltiHQ/control-plane/internal/cluster"
)

// New builds the driver cfg selects: static, the default, with the peers of
// [cluster.DiscoveryConfig.StaticPeers], or dns.
func New(cfg cluster.DiscoveryConfig) (cluster.Discovery, error) {
	switch cfg.Driver {
	case "", cluster.DriverStatic:
		peers, err := cfg.StaticPeers()
		if err != nil {
			return nil, err
		}
		return NewStatic(peers), nil
	case cluster.DriverDNS:
		return NewDNS(cfg.Hostname, cfg.Port)
	default:
		return nil, fmt.Errorf("discovery: unknown driver %q", cfg.Driver)
	}
}
