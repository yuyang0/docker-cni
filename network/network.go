package network

import (
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/projecteru2/docker-cni/store"
)

type Network interface {
	SimulateCNIAdd(info *store.InterfaceInfo, state *specs.State) error
}
