package network

import (
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/projecteru2/docker-cni/config"
	"github.com/projecteru2/docker-cni/store"
)

type Network interface {
	ExtractNetworkInfo(conf *config.Config, state *specs.State) (*store.InterfaceInfo, error)
	SimulateCNIAdd(info *store.InterfaceInfo, state *specs.State) error
}
