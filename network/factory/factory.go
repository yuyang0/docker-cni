package factory

import (
	"fmt"
	"strings"

	"github.com/projecteru2/docker-cni/network"
	"github.com/projecteru2/docker-cni/network/calico"
)

func NewNetwork(networkType string) (network.Network, error) {
	switch strings.ToLower(networkType) {
	case "calico":
		return calico.New(), nil
	default:
		return nil, fmt.Errorf("unsupported CNI type: %s", networkType)
	}
}
