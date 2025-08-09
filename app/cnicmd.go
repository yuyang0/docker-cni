package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/containernetworking/cni/pkg/types"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"github.com/projecteru2/docker-cni/cni"
	"github.com/projecteru2/docker-cni/config"
	"github.com/projecteru2/docker-cni/handler"
	nwFact "github.com/projecteru2/docker-cni/network/factory"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

func runCNI(handler handler.Handler) func(*cli.Context) error {
	return func(c *cli.Context) (err error) {
		defer func() {
			if err != nil {
				log.Errorf("[hook] failed to preceed: %+v", err)
			}
		}()

		conf, err := config.LoadConfig(c.String("config"))
		if err != nil {
			return errors.WithStack(err)
		}

		if err = conf.SetupLog(); err != nil {
			return errors.WithStack(err)
		}
		if err := initStore(conf); err != nil {
			return errors.WithStack(err)
		}
		defer stor.Close()

		stateBuf, err := io.ReadAll(os.Stdin)
		if err != nil {
			return errors.WithStack(err)
		}
		var state specs.State
		if err = json.Unmarshal(stateBuf, &state); err != nil {
			return errors.WithStack(err)
		}

		file, err := os.OpenFile(conf.CNILog, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return errors.WithStack(err)
		}
		if err := syscall.Dup2(int(file.Fd()), 1); err != nil {
			return errors.WithStack(err)
		}
		if err := syscall.Dup2(int(file.Fd()), 2); err != nil {
			return errors.WithStack(err)
		}

		cmd := c.String("command")

		if conf.FixedIP {
			switch strings.ToUpper(cmd) {
			case "ADD":
				// trigger CLEAN task. if encounter error, just log it and continue
				if err = HandleClean(handler, conf); err != nil {
					log.Errorf("[hook] failed to clean up: %+v", err)
				}
				// in order to implement fixed ip, we don't run DEL command when stop container
				// so when start container next time, the ADD commnd will do nothing(CNI behavior)
				// and we need to configure the network manually
				// 1. store the interface information(container and hsot veth name, ip) in db
				// 2. when start container, we need create veth pair and configure ip and gateway manually
				st, err := stor.GetContainerState(state.ID)
				if err != nil {
					log.Errorf("[hook] failed to get container state: %+v", err)
					return errors.WithStack(err)
				}

				nw, err := nwFact.NewNetwork(conf.CNIType)
				if err != nil {
					log.Errorf("[hook] failed to create network object: %v", err)
					return errors.WithStack(err)
				}

				// create a new container
				if st == nil {
					res, err := runCNICommand(handler, conf, &state, cmd)
					if err != nil {
						log.Errorf("[hook] failed to run CNI ADD: %+v", err)
						return errors.WithStack(err)
					}

					// Store CNI result
					var buf bytes.Buffer
					if err = res.PrintTo(&buf); err != nil {
						log.Errorf("[hook] failed to marshal CNI result: %+v", err)
						return errors.WithStack(err)
					}
					log.Infof("[hook] CNI ADD result: %s", buf.String())
					info, err := nw.ExtractNetworkInfo(&conf, &state)
					if err != nil {
						log.Errorf("[hook] failed to extract network info: %+v", err)
						return errors.WithStack(err)
					}
					log.Infof("[hook] extracted network info: %+v", info)
					if err = stor.PutInterfaceInfo(state.ID, info); err != nil {
						log.Errorf("[hook] failed to store CNI result: %+v", err)
						return errors.WithStack(err)
					}

					if err = stor.PutContainerState(state.ID, &state); err != nil {
						log.Errorf("[hook] failed to store container state: %+v", err)
						return errors.WithStack(err)
					}
					return nil
				}

				// start an old container
				info, err := stor.GetInterfaceInfo(state.ID)
				if err != nil {
					log.Errorf("[hook] failed to get interface info: %+v", err)
					return errors.WithStack(err)
				}
				if err = nw.SimulateCNIAdd(info, &state); err != nil {
					log.Errorf("[hook] failed to simulate CNI ADD: %+v", err)
					return errors.WithStack(err)
				}
				return nil
			case "DEL":
				// for fixed IP, we don't release cni resource when container stopped
				// we just store the state in db and the CLEAN task will release the cni resources for removed containers
				// if err = stor.PutContainerState(state.ID, &state); err != nil {
				// 	return errors.WithStack(err)
				// }
				return nil
			}
		}
		_, err = runCNICommand(handler, conf, &state, cmd)
		return err
	}
}

func runCNICommand(handler handler.Handler, conf config.Config, state *specs.State, cmd string) (res types.Result, err error) {
	netns := ""
	if state.Pid != 0 {
		netns = fmt.Sprintf("/proc/%d/ns/net", state.Pid)
	}
	cniToolConfig := cni.CNIToolConfig{
		CNIPath:     conf.CNIBinDir,
		NetConfPath: conf.CNIConfDir,
		NetNS:       netns,
		Args:        os.Getenv("CNI_ARGS"),
		IfName:      conf.CNIIfname,
		Cmd:         cmd,
		ContainerID: state.ID,
		Handler:     handler.HandleCNIConfig,
	}

	log.Infof("[hook] docker-cni running: %+v", cniToolConfig)
	res, err = cni.Run(cniToolConfig)
	return res, errors.WithStack(err)
}
