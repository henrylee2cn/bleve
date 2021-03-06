//  Copyright (c) 2016 Couchbase, Inc.
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the
//  License. You may obtain a copy of the License at
//    http://www.apache.org/licenses/LICENSE-2.0
//  Unless required by applicable law or agreed to in writing,
//  software distributed under the License is distributed on an "AS
//  IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
//  express or implied. See the License for the specific language
//  governing permissions and limitations under the License.

// Package moss provides a KVStore implementation based on the
// github.com/couchbaselabs/moss library.

package moss

import (
	"fmt"
	"sync"

	"github.com/couchbase/moss"

	"github.com/blevesearch/bleve/index/store"
	"github.com/blevesearch/bleve/registry"
)

func initLowerLevelStore(
	mo store.MergeOperator,
	config map[string]interface{},
	lowerLevelStoreName string,
	lowerLevelStoreConfig map[string]interface{},
	lowerLevelMaxBatchSize uint64,
	logf func(format string, a ...interface{}),
) (moss.Snapshot, moss.LowerLevelUpdate, store.KVStore, error) {
	constructor := registry.KVStoreConstructorByName(lowerLevelStoreName)
	if constructor == nil {
		return nil, nil, nil, fmt.Errorf("moss store, initLowerLevelStore,"+
			" could not find lower level store: %s", lowerLevelStoreName)
	}

	if lowerLevelStoreConfig == nil {
		lowerLevelStoreConfig = map[string]interface{}{}
	}

	for k, v := range config {
		_, exists := lowerLevelStoreConfig[k]
		if !exists {
			lowerLevelStoreConfig[k] = v
		}
	}

	kvStore, err := constructor(mo, lowerLevelStoreConfig)
	if err != nil {
		return nil, nil, nil, err
	}

	llStore := &llStore{
		refs:     0,
		config:   config,
		llConfig: lowerLevelStoreConfig,
		kvStore:  kvStore,
		logf:     logf,
	}

	llUpdate := func(ssHigher moss.Snapshot) (ssLower moss.Snapshot, err error) {
		return llStore.update(ssHigher, lowerLevelMaxBatchSize)
	}

	llSnapshot, err := llUpdate(nil)
	if err != nil {
		_ = kvStore.Close()
		return nil, nil, nil, err
	}

	return llSnapshot, llUpdate, kvStore, nil // llStore.refs is now 1.
}

// ------------------------------------------------

// llStore is a lower level store and provides ref-counting around a
// bleve store.KVStore.
type llStore struct {
	kvStore store.KVStore

	config   map[string]interface{}
	llConfig map[string]interface{}

	logf func(format string, a ...interface{})

	m    sync.Mutex // Protects fields that follow.
	refs int
}

// llSnapshot represents a lower-level snapshot, wrapping a bleve
// store.KVReader, and implements the moss.Snapshot interface.
type llSnapshot struct {
	llStore  *llStore // Holds 1 refs on the llStore.
	kvReader store.KVReader

	m    sync.Mutex // Protects fields that follow.
	refs int
}

// llIterator represents a lower-level iterator, wrapping a bleve
// store.KVIterator, and implements the moss.Iterator interface.
type llIterator struct {
	llSnapshot *llSnapshot // Holds 1 refs on the llSnapshot.

	// Some lower-level KVReader implementations need a separate
	// KVReader clone, due to KVReader single-threaded'ness.
	kvReader store.KVReader

	kvIterator store.KVIterator
}

type readerSource interface {
	Reader() (store.KVReader, error)
}

// ------------------------------------------------

func (s *llStore) addRef() *llStore {
	s.m.Lock()
	s.refs += 1
	s.m.Unlock()

	return s
}

func (s *llStore) decRef() {
	s.m.Lock()
	s.refs -= 1
	if s.refs <= 0 {
		err := s.kvStore.Close()
		if err != nil {
			s.logf("llStore kvStore.Close err: %v", err)
		}
	}
	s.m.Unlock()
}

// update() mutates this lower level store with latest data from the
// given higher level moss.Snapshot and returns a new moss.Snapshot
// that the higher level can use which represents this lower level
// store.
func (s *llStore) update(ssHigher moss.Snapshot, maxBatchSize uint64) (
	ssLower moss.Snapshot, err error) {
	if ssHigher != nil {
		iter, err := ssHigher.StartIterator(nil, nil, moss.IteratorOptions{
			IncludeDeletions: true,
			SkipLowerLevel:   true,
		})
		if err != nil {
			return nil, err
		}

		defer func() {
			err = iter.Close()
			if err != nil {
				s.logf("llStore iter.Close err: %v", err)
			}
		}()

		kvWriter, err := s.kvStore.Writer()
		if err != nil {
			return nil, err
		}

		defer func() {
			err = kvWriter.Close()
			if err != nil {
				s.logf("llStore kvWriter.Close err: %v", err)
			}
		}()

		batch := kvWriter.NewBatch()

		defer func() {
			if batch != nil {
				err = batch.Close()
				if err != nil {
					s.logf("llStore batch.Close err: %v", err)
				}
			}
		}()

		var readOptions moss.ReadOptions

		i := uint64(0)
		for {
			if i%1000000 == 0 {
				s.logf("llStore.update, i: %d", i)
			}

			ex, key, val, err := iter.CurrentEx()
			if err == moss.ErrIteratorDone {
				break
			}
			if err != nil {
				return nil, err
			}

			switch ex.Operation {
			case moss.OperationSet:
				batch.Set(key, val)

			case moss.OperationDel:
				batch.Delete(key)

			case moss.OperationMerge:
				val, err = ssHigher.Get(key, readOptions)
				if err != nil {
					return nil, err
				}

				if val != nil {
					batch.Set(key, val)
				} else {
					batch.Delete(key)
				}

			default:
				return nil, fmt.Errorf("moss store, update,"+
					" unexpected operation, ex: %v", ex)
			}

			i++

			err = iter.Next()
			if err == moss.ErrIteratorDone {
				break
			}
			if err != nil {
				return nil, err
			}

			if maxBatchSize > 0 && i%maxBatchSize == 0 {
				err = kvWriter.ExecuteBatch(batch)
				if err != nil {
					return nil, err
				}

				err = batch.Close()
				if err != nil {
					return nil, err
				}

				batch = kvWriter.NewBatch()
			}
		}

		if i > 0 {
			s.logf("llStore.update, ExecuteBatch,"+
				" path: %s, total: %d, start", s.llConfig["path"], i)

			err = kvWriter.ExecuteBatch(batch)
			if err != nil {
				return nil, err
			}

			s.logf("llStore.update, ExecuteBatch,"+
				" path: %s: total: %d, done", s.llConfig["path"], i)
		}
	}

	kvReader, err := s.kvStore.Reader()
	if err != nil {
		return nil, err
	}

	s.logf("llStore.update, new reader")

	return &llSnapshot{
		llStore:  s.addRef(),
		kvReader: kvReader,
		refs:     1,
	}, nil
}

// ------------------------------------------------

func (llss *llSnapshot) addRef() *llSnapshot {
	llss.m.Lock()
	llss.refs += 1
	llss.m.Unlock()

	return llss
}

func (llss *llSnapshot) decRef() {
	llss.m.Lock()
	llss.refs -= 1
	if llss.refs <= 0 {
		if llss.kvReader != nil {
			err := llss.kvReader.Close()
			if err != nil {
				llss.llStore.logf("llSnapshot kvReader.Close err: %v", err)
			}

			llss.kvReader = nil
		}

		if llss.llStore != nil {
			llss.llStore.decRef()
			llss.llStore = nil
		}
	}
	llss.m.Unlock()
}

func (llss *llSnapshot) Close() error {
	llss.decRef()

	return nil
}

func (llss *llSnapshot) Get(key []byte,
	readOptions moss.ReadOptions) ([]byte, error) {
	rs, ok := llss.kvReader.(readerSource)
	if ok {
		r2, err := rs.Reader()
		if err != nil {
			return nil, err
		}

		val, err := r2.Get(key)

		_ = r2.Close()

		return val, err
	}

	return llss.kvReader.Get(key)
}

func (llss *llSnapshot) StartIterator(
	startKeyInclusive, endKeyExclusive []byte,
	iteratorOptions moss.IteratorOptions) (moss.Iterator, error) {
	rs, ok := llss.kvReader.(readerSource)
	if ok {
		r2, err := rs.Reader()
		if err != nil {
			return nil, err
		}

		i2 := r2.RangeIterator(startKeyInclusive, endKeyExclusive)

		return &llIterator{llSnapshot: llss.addRef(), kvReader: r2, kvIterator: i2}, nil
	}

	i := llss.kvReader.RangeIterator(startKeyInclusive, endKeyExclusive)

	return &llIterator{llSnapshot: llss.addRef(), kvReader: nil, kvIterator: i}, nil
}

// ------------------------------------------------

func (lli *llIterator) Close() error {
	var err0 error
	if lli.kvIterator != nil {
		err0 = lli.kvIterator.Close()
		lli.kvIterator = nil
	}

	var err1 error
	if lli.kvReader != nil {
		err1 = lli.kvReader.Close()
		lli.kvReader = nil
	}

	lli.llSnapshot.decRef()
	lli.llSnapshot = nil

	if err0 != nil {
		return err0
	}

	if err1 != nil {
		return err1
	}

	return nil
}

func (lli *llIterator) Next() error {
	lli.kvIterator.Next()

	return nil
}

func (lli *llIterator) Current() (key, val []byte, err error) {
	key, val, ok := lli.kvIterator.Current()
	if !ok {
		return nil, nil, moss.ErrIteratorDone
	}

	return key, val, nil
}

func (lli *llIterator) CurrentEx() (
	entryEx moss.EntryEx, key, val []byte, err error) {
	return moss.EntryEx{}, nil, nil, moss.ErrUnimplemented

}
