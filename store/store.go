package store

import (
	"github.com/opencontainers/runtime-spec/specs-go"
)

type InterfaceInfo struct {
	IFName     string   `json:"ifname"`                // interface name in container netns
	HostIFName string   `json:"host_ifname,omitempty"` // host side veth name
	MAC        string   `json:"mac,omitempty"`         // MAC address of the container interface
	IPs        []string `json:"ips"`
	Routes     []string `json:"routes"`
}

type Store interface {
	Open() error
	Close() error
	PutInterfaceInfo(key string, info *InterfaceInfo) error
	GetInterfaceInfo(key string) (*InterfaceInfo, error)

	PutContainerState(id string, state *specs.State) error
	GetContainerState(id string) (*specs.State, error)

	DeleteContiners(existContainerIDs map[string]struct{}) (map[string]specs.State, error)
}
