package bbolt

import (
	"encoding/json"
	"time"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/projecteru2/docker-cni/config"
	"github.com/projecteru2/docker-cni/store"
	bolt "go.etcd.io/bbolt"

	"github.com/pkg/errors"
)

const (
	stateBucketName     = "docker-cni-state"
	addOutputBucketName = "docker-cni-add-output"
)

type Store struct {
	conf config.Config
	db   *bolt.DB
}

func New(conf config.Config) *Store {
	store := &Store{
		conf: conf,
		db:   nil,
	}
	return store
}

func (s *Store) Open() error {
	if s.db != nil {
		return nil // Already opened
	}
	conf := &s.conf
	var err error

	s.db, err = bolt.Open(conf.StoreFile, 0600, &bolt.Options{Timeout: 30 * time.Second})
	return err
}

func (s *Store) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *Store) PutInterfaceInfo(key string, info *store.InterfaceInfo) error {
	infoBuf, err := json.Marshal(info)
	if err != nil {
		return errors.WithStack(err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		var err error
		b, err := tx.CreateBucketIfNotExists([]byte(addOutputBucketName))
		if err != nil {
			return errors.WithStack(err)
		}
		return b.Put([]byte(key), infoBuf)
	})
}

func (s *Store) GetInterfaceInfo(key string) (*store.InterfaceInfo, error) {
	var info *store.InterfaceInfo
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(addOutputBucketName))
		if b == nil {
			return nil // No results found
		}
		infoBytes := b.Get([]byte(key))
		if len(infoBytes) == 0 {
			return nil
		}
		info = &store.InterfaceInfo{}
		if err := json.Unmarshal(infoBytes, info); err != nil {
			return errors.WithStack(err)
		}
		return nil
	})
	return info, errors.WithStack(err)
}

func (s *Store) PutContainerState(id string, state *specs.State) error {
	stateBuf, err := json.Marshal(state)
	if err != nil {
		return errors.WithStack(err)
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(stateBucketName))
		if err != nil {
			return errors.WithStack(err)
		}
		return b.Put([]byte(id), stateBuf)
	})
}

func (s *Store) GetContainerState(id string) (*specs.State, error) {
	var state *specs.State
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(stateBucketName))
		if b == nil {
			return nil // No state found
		}
		stateBuf := b.Get([]byte(id))
		if stateBuf == nil {
			return nil // No state found for this ID
		}
		state = &specs.State{}
		return json.Unmarshal(stateBuf, state)
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return state, nil
}

func (s *Store) DeleteContiners(existContainerIDs map[string]struct{}) (map[string]specs.State, error) {
	var err error
	deleteMap := make(map[string]specs.State)
	if err = s.db.Update(func(tx *bolt.Tx) error {
		b1 := tx.Bucket([]byte(stateBucketName))
		if b1 == nil {
			return nil
		}
		b2 := tx.Bucket([]byte(addOutputBucketName))
		if b2 == nil {
			return nil
		}

		cursor := b1.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			// Delete key if container no longer exists
			if _, exists := existContainerIDs[string(k)]; !exists {
				if err := b1.Delete(k); err != nil {
					return err
				}
				if err := b2.Delete(k); err != nil {
					return err
				}

				var state specs.State
				if err = json.Unmarshal(v, &state); err != nil {
					return errors.WithStack(err)
				}
				deleteMap[string(k)] = state
			}
		}
		return nil
	}); err != nil {
		return nil, errors.WithStack(err)
	}
	return deleteMap, nil
}
