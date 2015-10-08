package datastore

import (
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"

	"github.com/docker/libkv"
	"github.com/docker/libkv/store"
	"github.com/docker/libkv/store/boltdb"
	"github.com/docker/libkv/store/consul"
	"github.com/docker/libkv/store/etcd"
	"github.com/docker/libkv/store/zookeeper"
	"github.com/docker/libnetwork/types"
)

//DataStore exported
type DataStore interface {
	// GetObject gets data from datastore and unmarshals to the specified object
	GetObject(key string, o KVObject) error
	// PutObject adds a new Record based on an object into the datastore
	PutObject(kvObject KVObject) error
	// PutObjectAtomic provides an atomic add and update operation for a Record
	PutObjectAtomic(kvObject KVObject) error
	// DeleteObject deletes a record
	DeleteObject(kvObject KVObject) error
	// DeleteObjectAtomic performs an atomic delete operation
	DeleteObjectAtomic(kvObject KVObject) error
	// DeleteTree deletes a record
	DeleteTree(kvObject KVObject) error
	// Watchable returns whether the store is watchable are not
	Watchable() bool
	// Watch for changes on a KVObject
	Watch(kvObject KVObject, stopCh <-chan struct{}) (<-chan KVObject, error)
	// List returns of a list of KVObjects belonging to the parent
	// key. The caller must pass a KVObject of the same type as
	// the objects that need to be listed
	List(string, KVObject) ([]KVObject, error)
	// Scope returns the scope of the store
	Scope() string
	// KVStore returns access to the KV Store
	KVStore() store.Store
	// Close closes the data store
	Close()
}

// ErrKeyModified is raised for an atomic update when the update is working on a stale state
var (
	ErrKeyModified = store.ErrKeyModified
	ErrKeyNotFound = store.ErrKeyNotFound
)

type datastore struct {
	scope string
	store store.Store
	cache *cache
	cfg   ScopeCfg
}

// KVObject is  Key/Value interface used by objects to be part of the DataStore
type KVObject interface {
	// Key method lets an object to provide the Key to be used in KV Store
	Key() []string
	// KeyPrefix method lets an object to return immediate parent key that can be used for tree walk
	KeyPrefix() []string
	// Value method lets an object to marshal its content to be stored in the KV store
	Value() []byte
	// SetValue is used by the datastore to set the object's value when loaded from the data store.
	SetValue([]byte) error
	// Index method returns the latest DB Index as seen by the object
	Index() uint64
	// SetIndex method allows the datastore to store the latest DB Index into the object
	SetIndex(uint64)
	// True if the object exists in the datastore, false if it hasn't been stored yet.
	// When SetIndex() is called, the object has been stored.
	Exists() bool
	// DataScope indicates the storage scope of the KV object
	DataScope() string
	// Skip provides a way for a KV Object to avoid persisting it in the KV Store
	Skip() bool
}

// KVConstructor interface defines methods which can construct a KVObject from another.
type KVConstructor interface {
	// New returns a new object which is created based on the
	// source object
	New() KVObject
	// CopyTo deep copies the contents of the implementing object
	// to the passed destination object
	CopyTo(KVObject) error
}

// ScopeCfg represents Datastore configuration.
type ScopeCfg struct {
	Embedded bool
	Client   ScopeClientCfg
}

// ScopeClientCfg represents Datastore Client-only mode configuration
type ScopeClientCfg struct {
	Provider string
	Address  string
	Config   *store.Config
}

type storeTableData struct {
	refCnt int
	store  store.Store
	once   sync.Once
}

const (
	// LocalScope indicates to store the KV object in local datastore such as boltdb
	LocalScope = "local"
	// GlobalScope indicates to store the KV object in global datastore such as consul/etcd/zookeeper
	GlobalScope   = "global"
	defaultPrefix = "/var/lib/docker/network/files"
)

const (
	// NetworkKeyPrefix is the prefix for network key in the kv store
	NetworkKeyPrefix = "network"
	// EndpointKeyPrefix is the prefix for endpoint key in the kv store
	EndpointKeyPrefix = "endpoint"
)

var (
	defaultScopes = makeDefaultScopes()
	storeLock     sync.Mutex
	storeTable    = make(map[ScopeCfg]*storeTableData)
)

func makeDefaultScopes() map[string]*ScopeCfg {
	def := make(map[string]*ScopeCfg)
	def[LocalScope] = &ScopeCfg{
		Embedded: true,
		Client: ScopeClientCfg{
			Provider: "boltdb",
			Address:  defaultPrefix + "/boltdb.db",
			Config: &store.Config{
				Bucket: "libnetwork",
			},
		},
	}

	return def
}

var rootChain = []string{"docker", "libnetwork"}

func init() {
	consul.Register()
	zookeeper.Register()
	etcd.Register()
	boltdb.Register()
}

// DefaultScopes returns a map of default scopes and it's config for clients to use.
func DefaultScopes(dataDir string) map[string]*ScopeCfg {
	if dataDir != "" {
		defaultScopes[LocalScope].Client.Address = dataDir + "/network/files/boltdb.db"
		return defaultScopes
	}

	defaultScopes[LocalScope].Client.Address = defaultPrefix + "/boltdb.db"
	return defaultScopes
}

// IsValid checks if the scope config has valid configuration.
func (cfg *ScopeCfg) IsValid() bool {
	if cfg == nil ||
		strings.TrimSpace(cfg.Client.Provider) == "" ||
		strings.TrimSpace(cfg.Client.Address) == "" {
		return false
	}

	return true
}

//Key provides convenient method to create a Key
func Key(key ...string) string {
	keychain := append(rootChain, key...)
	str := strings.Join(keychain, "/")
	return str + "/"
}

//ParseKey provides convenient method to unpack the key to complement the Key function
func ParseKey(key string) ([]string, error) {
	chain := strings.Split(strings.Trim(key, "/"), "/")

	// The key must atleast be equal to the rootChain in order to be considered as valid
	if len(chain) <= len(rootChain) || !reflect.DeepEqual(chain[0:len(rootChain)], rootChain) {
		return nil, types.BadRequestErrorf("invalid Key : %s", key)
	}
	return chain[len(rootChain):], nil
}

// newClient used to connect to KV Store
func newClient(scope string, kv string, addrs string, config *store.Config, cached bool) (*datastore, error) {
	if config == nil {
		config = &store.Config{}
	}
	store, err := libkv.NewStore(store.Backend(kv), []string{addrs}, config)
	if err != nil {
		return nil, err
	}

	ds := &datastore{scope: scope, store: store}
	if cached {
		ds.cache = newCache(ds)
	}

	return ds, nil
}

// NewDataStore creates a new instance of LibKV data store
func NewDataStore(scope string, cfg *ScopeCfg) (DataStore, error) {
	var (
		err error
		ds  *datastore
	)

	if !cfg.IsValid() {
		return nil, fmt.Errorf("invalid datastore configuration passed for scope %s", scope)
	}

	storeLock.Lock()
	sdata, ok := storeTable[*cfg]
	if ok {
		sdata.refCnt++
		// If sdata already has a store nothing to do. Just
		// create a datastore handle using it and return with
		// that.
		if sdata.store != nil {
			storeLock.Unlock()
			return &datastore{scope: scope, cfg: *cfg, store: sdata.store}, nil
		}
	} else {
		// If sdata is not present create one and add ito
		// storeTable while holding the lock.
		sdata = &storeTableData{refCnt: 1}
		storeTable[*cfg] = sdata
	}
	storeLock.Unlock()

	// We come here either because:
	//
	// 1. We just created the store table data OR
	// 2. We picked up the store table data from table but store was not initialized.
	//
	// In both cases the once function will ensure the store
	// initialization happens exactly once
	sdata.once.Do(func() {
		ds, err = newClient(scope, cfg.Client.Provider, cfg.Client.Address, cfg.Client.Config, scope == LocalScope)
		if err != nil {
			return
		}

		ds.cfg = *cfg
		sdata.store = ds.store
	})

	if err != nil {
		return nil, err
	}

	return ds, nil
}

func (ds *datastore) Close() {
	storeLock.Lock()
	sdata := storeTable[ds.cfg]

	if sdata == nil {
		storeLock.Unlock()
		return
	}

	sdata.refCnt--
	if sdata.refCnt > 0 {
		storeLock.Unlock()
		return
	}

	delete(storeTable, ds.cfg)
	storeLock.Unlock()

	ds.store.Close()
}

func (ds *datastore) Scope() string {
	return ds.scope
}

func (ds *datastore) Watchable() bool {
	return ds.scope != LocalScope
}

func (ds *datastore) Watch(kvObject KVObject, stopCh <-chan struct{}) (<-chan KVObject, error) {
	sCh := make(chan struct{})

	ctor, ok := kvObject.(KVConstructor)
	if !ok {
		return nil, fmt.Errorf("error watching object type %T, object does not implement KVConstructor interface", kvObject)
	}

	kvpCh, err := ds.store.Watch(Key(kvObject.Key()...), sCh)
	if err != nil {
		return nil, err
	}

	kvoCh := make(chan KVObject)

	go func() {
		for {
			select {
			case <-stopCh:
				close(sCh)
				return
			case kvPair := <-kvpCh:
				dstO := ctor.New()

				if err := dstO.SetValue(kvPair.Value); err != nil {
					log.Printf("Could not unmarshal kvpair value = %s", string(kvPair.Value))
					break
				}

				dstO.SetIndex(kvPair.LastIndex)
				kvoCh <- dstO
			}
		}
	}()

	return kvoCh, nil
}

func (ds *datastore) KVStore() store.Store {
	return ds.store
}

// PutObjectAtomic adds a new Record based on an object into the datastore
func (ds *datastore) PutObjectAtomic(kvObject KVObject) error {
	var (
		previous *store.KVPair
		pair     *store.KVPair
		err      error
	)

	if kvObject == nil {
		return types.BadRequestErrorf("invalid KV Object : nil")
	}

	kvObjValue := kvObject.Value()

	if kvObjValue == nil {
		return types.BadRequestErrorf("invalid KV Object with a nil Value for key %s", Key(kvObject.Key()...))
	}

	if kvObject.Skip() {
		goto add_cache
	}

	if kvObject.Exists() {
		previous = &store.KVPair{Key: Key(kvObject.Key()...), LastIndex: kvObject.Index()}
	} else {
		previous = nil
	}

	_, pair, err = ds.store.AtomicPut(Key(kvObject.Key()...), kvObjValue, previous, nil)
	if err != nil {
		return err
	}

	kvObject.SetIndex(pair.LastIndex)

add_cache:
	if ds.cache != nil {
		return ds.cache.add(kvObject)
	}

	return nil
}

// PutObject adds a new Record based on an object into the datastore
func (ds *datastore) PutObject(kvObject KVObject) error {
	if kvObject == nil {
		return types.BadRequestErrorf("invalid KV Object : nil")
	}

	if kvObject.Skip() {
		goto add_cache
	}

	if err := ds.putObjectWithKey(kvObject, kvObject.Key()...); err != nil {
		return err
	}

add_cache:
	if ds.cache != nil {
		return ds.cache.add(kvObject)
	}

	return nil
}

func (ds *datastore) putObjectWithKey(kvObject KVObject, key ...string) error {
	kvObjValue := kvObject.Value()

	if kvObjValue == nil {
		return types.BadRequestErrorf("invalid KV Object with a nil Value for key %s", Key(kvObject.Key()...))
	}
	return ds.store.Put(Key(key...), kvObjValue, nil)
}

// GetObject returns a record matching the key
func (ds *datastore) GetObject(key string, o KVObject) error {
	if ds.cache != nil {
		return ds.cache.get(key, o)
	}

	kvPair, err := ds.store.Get(key)
	if err != nil {
		return err
	}

	if err := o.SetValue(kvPair.Value); err != nil {
		return err
	}

	// Make sure the object has a correct view of the DB index in
	// case we need to modify it and update the DB.
	o.SetIndex(kvPair.LastIndex)
	return nil
}

func (ds *datastore) ensureKey(key string) error {
	exists, err := ds.store.Exists(key)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return ds.store.Put(key, []byte{}, nil)
}

func (ds *datastore) List(key string, kvObject KVObject) ([]KVObject, error) {
	if ds.cache != nil {
		return ds.cache.list(kvObject)
	}

	// Bail out right away if the kvObject does not implement KVConstructor
	ctor, ok := kvObject.(KVConstructor)
	if !ok {
		return nil, fmt.Errorf("error listing objects, object does not implement KVConstructor interface")
	}

	// Make sure the parent key exists
	if err := ds.ensureKey(key); err != nil {
		return nil, err
	}

	kvList, err := ds.store.List(key)
	if err != nil {
		return nil, err
	}

	var kvol []KVObject
	for _, kvPair := range kvList {
		if len(kvPair.Value) == 0 {
			continue
		}

		dstO := ctor.New()
		if err := dstO.SetValue(kvPair.Value); err != nil {
			return nil, err
		}

		// Make sure the object has a correct view of the DB index in
		// case we need to modify it and update the DB.
		dstO.SetIndex(kvPair.LastIndex)

		kvol = append(kvol, dstO)
	}

	return kvol, nil
}

// DeleteObject unconditionally deletes a record from the store
func (ds *datastore) DeleteObject(kvObject KVObject) error {
	// cleaup the cache first
	if ds.cache != nil {
		ds.cache.del(kvObject)
	}

	if kvObject.Skip() {
		return nil
	}

	return ds.store.Delete(Key(kvObject.Key()...))
}

// DeleteObjectAtomic performs atomic delete on a record
func (ds *datastore) DeleteObjectAtomic(kvObject KVObject) error {
	if kvObject == nil {
		return types.BadRequestErrorf("invalid KV Object : nil")
	}

	previous := &store.KVPair{Key: Key(kvObject.Key()...), LastIndex: kvObject.Index()}

	if kvObject.Skip() {
		goto del_cache
	}

	if _, err := ds.store.AtomicDelete(Key(kvObject.Key()...), previous); err != nil {
		return err
	}

del_cache:
	// cleanup the cache only if AtomicDelete went through successfully
	if ds.cache != nil {
		return ds.cache.del(kvObject)
	}

	return nil
}

// DeleteTree unconditionally deletes a record from the store
func (ds *datastore) DeleteTree(kvObject KVObject) error {
	// cleaup the cache first
	if ds.cache != nil {
		ds.cache.del(kvObject)
	}

	if kvObject.Skip() {
		return nil
	}

	return ds.store.DeleteTree(Key(kvObject.KeyPrefix()...))
}
