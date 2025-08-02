package app

import (
	"os"

	"github.com/pkg/errors"
	"github.com/projecteru2/docker-cni/config"
	"github.com/projecteru2/docker-cni/handler"
	"github.com/projecteru2/docker-cni/store"
	"github.com/projecteru2/docker-cni/store/bbolt"
	log "github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

var (
	stor store.Store
)

func initStore(conf config.Config) error {
	if stor == nil {
		stor = bbolt.New(conf)
		if err := stor.Open(); err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func runClean(handler handler.Handler) func(*cli.Context) error {
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

		log.Info("[hook] docker-cni running clean")
		err = HandleClean(handler, conf)
		return errors.WithStack(err)
	}
}

func HandleClean(handler handler.Handler, conf config.Config) (err error) {
	// Get existing container IDs as a map
	containerIDs, err := getDockerContainerIDMap()
	if err != nil {
		return err
	}

	deleteMap, err := stor.DeleteContiners(containerIDs)
	if err != nil {
		return errors.WithStack(err)
	}
	var err2 error
	for id, state := range deleteMap {
		log.Infof("[hook] cleaning up CNI resource for container %s", id)
		if _, err = runCNICommand(handler, conf, &state, "del"); err != nil {
			log.Errorf("[hook] failed to clean up container %s's CNI resources: %v", id, err)
			err2 = err
		}
	}

	return err2
}

func getDockerContainerIDMap() (map[string]struct{}, error) {
	containerPath := "/var/lib/docker/containers"
	files, err := os.ReadDir(containerPath)
	if err != nil {
		return nil, err
	}

	// Using empty struct{} as value since we only care about existence
	containerIDs := make(map[string]struct{})
	for _, f := range files {
		if f.IsDir() {
			containerIDs[f.Name()] = struct{}{}
		}
	}
	return containerIDs, nil
}
