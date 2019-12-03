package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/gorilla/mux"

	lxd "github.com/lxc/lxd/client"
	"github.com/lxc/lxd/lxd/cluster"
	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/response"
	storagePools "github.com/lxc/lxd/lxd/storage"
	storageDrivers "github.com/lxc/lxd/lxd/storage/drivers"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/version"
)

// Lock to prevent concurent storage pools creation
var storagePoolCreateLock sync.Mutex

var storagePoolsCmd = APIEndpoint{
	Path: "storage-pools",

	Get:  APIEndpointAction{Handler: storagePoolsGet, AccessHandler: AllowAuthenticated},
	Post: APIEndpointAction{Handler: storagePoolsPost},
}

var storagePoolCmd = APIEndpoint{
	Path: "storage-pools/{name}",

	Delete: APIEndpointAction{Handler: storagePoolDelete},
	Get:    APIEndpointAction{Handler: storagePoolGet, AccessHandler: AllowAuthenticated},
	Patch:  APIEndpointAction{Handler: storagePoolPatch},
	Put:    APIEndpointAction{Handler: storagePoolPut},
}

// /1.0/storage-pools
// List all storage pools.
func storagePoolsGet(d *Daemon, r *http.Request) response.Response {
	recursion := util.IsRecursionRequest(r)

	pools, err := d.cluster.StoragePools()
	if err != nil && err != db.ErrNoSuchObject {
		return response.SmartError(err)
	}

	resultString := []string{}
	resultMap := []api.StoragePool{}
	for _, pool := range pools {
		if !recursion {
			resultString = append(resultString, fmt.Sprintf("/%s/storage-pools/%s", version.APIVersion, pool))
		} else {
			plID, pl, err := d.cluster.StoragePoolGet(pool)
			if err != nil {
				continue
			}

			// Get all users of the storage pool.
			poolUsedBy, err := storagePoolUsedByGet(d.State(), plID, pool)
			if err != nil {
				return response.SmartError(err)
			}
			pl.UsedBy = poolUsedBy

			resultMap = append(resultMap, *pl)
		}
	}

	if !recursion {
		return response.SyncResponse(true, resultString)
	}

	return response.SyncResponse(true, resultMap)
}

// /1.0/storage-pools
// Create a storage pool.
func storagePoolsPost(d *Daemon, r *http.Request) response.Response {
	storagePoolCreateLock.Lock()
	defer storagePoolCreateLock.Unlock()

	req := api.StoragePoolsPost{}

	// Parse the request.
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Sanity checks.
	if req.Name == "" {
		return response.BadRequest(fmt.Errorf("No name provided"))
	}

	if strings.Contains(req.Name, "/") {
		return response.BadRequest(fmt.Errorf("Storage pool names may not contain slashes"))
	}

	if req.Driver == "" {
		return response.BadRequest(fmt.Errorf("No driver provided"))
	}

	url := fmt.Sprintf("/%s/storage-pools/%s", version.APIVersion, req.Name)
	resp := response.SyncResponseLocation(true, nil, url)

	if isClusterNotification(r) {
		// This is an internal request which triggers the actual
		// creation of the pool across all nodes, after they have been
		// previously defined.
		err = storagePoolValidate(req.Name, req.Driver, req.Config)
		if err != nil {
			return response.BadRequest(err)
		}

		poolID, err := d.cluster.StoragePoolGetID(req.Name)
		if err != nil {
			return response.NotFound(err)
		}

		_, err = storagePoolCreateLocal(d.State(), poolID, req, true)
		if err != nil {
			return response.SmartError(err)
		}

		return resp
	}

	targetNode := queryParam(r, "target")
	if targetNode == "" {
		count, err := cluster.Count(d.State())
		if err != nil {
			return response.SmartError(err)
		}

		if count == 1 {
			// No targetNode was specified and we're either a single-node
			// cluster or not clustered at all, so create the storage
			// pool immediately.
			err = storagePoolCreateGlobal(d.State(), req)
		} else {
			// No targetNode was specified and we're clustered, so finalize the
			// config in the db and actually create the pool on all nodes.
			err = storagePoolsPostCluster(d, req)
		}
		if err != nil {
			return response.InternalError(err)
		}

		return resp
	}

	// A targetNode was specified, let's just define the node's storage
	// without actually creating it. The only legal key values for the
	// storage config are the ones in StoragePoolNodeConfigKeys.
	for key := range req.Config {
		if !shared.StringInSlice(key, db.StoragePoolNodeConfigKeys) {
			return response.SmartError(fmt.Errorf("Config key '%s' may not be used as node-specific key", key))
		}
	}

	err = storagePoolValidate(req.Name, req.Driver, req.Config)
	if err != nil {
		return response.BadRequest(err)
	}

	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		return tx.StoragePoolCreatePending(targetNode, req.Name, req.Driver, req.Config)
	})
	if err != nil {
		if err == db.ErrAlreadyDefined {
			return response.BadRequest(fmt.Errorf("The storage pool already defined on node %s", targetNode))
		}

		return response.SmartError(err)
	}

	return resp
}

func storagePoolsPostCluster(d *Daemon, req api.StoragePoolsPost) error {
	// Check that no node-specific config key has been defined.
	for key := range req.Config {
		if shared.StringInSlice(key, db.StoragePoolNodeConfigKeys) {
			return fmt.Errorf("Config key '%s' is node-specific", key)
		}
	}

	// Check that the pool is properly defined, fetch the node-specific
	// configs and insert the global config.
	var configs map[string]map[string]string
	var nodeName string
	var poolID int64
	err := d.cluster.Transaction(func(tx *db.ClusterTx) error {
		var err error

		// Check that the pool was defined at all.
		poolID, err = tx.StoragePoolID(req.Name)
		if err != nil {
			return err
		}

		// Fetch the node-specific configs.
		configs, err = tx.StoragePoolNodeConfigs(poolID)
		if err != nil {
			return err
		}

		// Take note of the name of this node
		nodeName, err = tx.NodeName()
		if err != nil {
			return err
		}

		// Insert the global config keys.
		return tx.StoragePoolConfigAdd(poolID, 0, req.Config)
	})
	if err != nil {
		if err == db.ErrNoSuchObject {
			return fmt.Errorf("Pool not pending on any node (use --target <node> first)")
		}
		return err
	}

	// Create the pool on this node.
	nodeReq := req
	for key, value := range configs[nodeName] {
		nodeReq.Config[key] = value
	}

	err = storagePoolValidate(req.Name, req.Driver, req.Config)
	if err != nil {
		return err
	}

	updatedConfig, err := storagePoolCreateLocal(d.State(), poolID, req, false)
	if err != nil {
		return err
	}
	req.Config = updatedConfig

	// Strip local config keys from config.
	for _, k := range db.StoragePoolNodeConfigKeys {
		delete(req.Config, k)
	}

	// Notify all other nodes to create the pool.
	notifier, err := cluster.NewNotifier(d.State(), d.endpoints.NetworkCert(), cluster.NotifyAll)
	if err != nil {
		return err
	}
	notifyErr := notifier(func(client lxd.InstanceServer) error {
		server, _, err := client.GetServer()
		if err != nil {
			return err
		}

		nodeReq := req
		for key, value := range configs[server.Environment.ServerName] {
			nodeReq.Config[key] = value
		}

		return client.CreateStoragePool(nodeReq)
	})

	errored := notifyErr != nil

	// Finally update the storage pool state.
	err = d.cluster.Transaction(func(tx *db.ClusterTx) error {
		if errored {
			return tx.StoragePoolErrored(req.Name)
		}
		return tx.StoragePoolCreated(req.Name)
	})
	if err != nil {
		return err
	}

	return notifyErr
}

// /1.0/storage-pools/{name}
// Get a single storage pool.
func storagePoolGet(d *Daemon, r *http.Request) response.Response {
	// If a target was specified, forward the request to the relevant node.
	resp := ForwardedResponseIfTargetIsRemote(d, r)
	if resp != nil {
		return resp
	}

	poolName := mux.Vars(r)["name"]

	// Get the existing storage pool.
	poolID, pool, err := d.cluster.StoragePoolGet(poolName)
	if err != nil {
		return response.SmartError(err)
	}

	// Get all users of the storage pool.
	poolUsedBy, err := storagePoolUsedByGet(d.State(), poolID, poolName)
	if err != nil && err != db.ErrNoSuchObject {
		return response.SmartError(err)
	}
	pool.UsedBy = poolUsedBy

	targetNode := queryParam(r, "target")

	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}

	// If no target node is specified and the daemon is clustered, we omit
	// the node-specific fields.
	if targetNode == "" && clustered {
		for _, key := range db.StoragePoolNodeConfigKeys {
			delete(pool.Config, key)
		}
	}

	etag := []interface{}{pool.Name, pool.Driver, pool.Config}

	return response.SyncResponseETag(true, &pool, etag)
}

// /1.0/storage-pools/{name}
// Replace pool properties.
func storagePoolPut(d *Daemon, r *http.Request) response.Response {
	poolName := mux.Vars(r)["name"]

	// Get the existing storage pool.
	_, dbInfo, err := d.cluster.StoragePoolGet(poolName)
	if err != nil {
		return response.SmartError(err)
	}

	req := api.StoragePoolPut{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return response.BadRequest(err)
	}

	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}

	config := dbInfo.Config
	if clustered {
		err := storagePoolValidateClusterConfig(req.Config)
		if err != nil {
			return response.BadRequest(err)
		}
		config = storagePoolClusterConfigForEtag(config)
	}

	// Validate the ETag
	etag := []interface{}{dbInfo.Name, dbInfo.Driver, config}

	err = util.EtagCheck(r, etag)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	// Validate the configuration
	err = storagePoolValidateConfig(poolName, dbInfo.Driver, req.Config, dbInfo.Config)
	if err != nil {
		return response.BadRequest(err)
	}

	config = req.Config
	if clustered {
		// For clustered requests, we need to complement the request's config
		// with our node-specific values.
		config = storagePoolClusterFillWithNodeConfig(dbInfo.Config, config)
	}

	// Notify the other nodes, unless this is itself a notification.
	if clustered && !isClusterNotification(r) {
		cert := d.endpoints.NetworkCert()
		notifier, err := cluster.NewNotifier(d.State(), cert, cluster.NotifyAll)
		if err != nil {
			return response.SmartError(err)
		}
		err = notifier(func(client lxd.InstanceServer) error {
			return client.UpdateStoragePool(poolName, req, r.Header.Get("If-Match"))
		})
		if err != nil {
			return response.SmartError(err)
		}
	}

	withDB := !isClusterNotification(r)
	err = storagePoolUpdate(d.State(), poolName, req.Description, config, withDB)
	if err != nil {
		return response.InternalError(err)
	}

	return response.EmptySyncResponse
}

// /1.0/storage-pools/{name}
// Change pool properties.
func storagePoolPatch(d *Daemon, r *http.Request) response.Response {
	poolName := mux.Vars(r)["name"]

	// Get the existing network
	_, dbInfo, err := d.cluster.StoragePoolGet(poolName)
	if err != nil {
		return response.SmartError(err)
	}

	req := api.StoragePoolPut{}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return response.BadRequest(err)
	}

	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}

	config := dbInfo.Config
	if clustered {
		err := storagePoolValidateClusterConfig(req.Config)
		if err != nil {
			return response.BadRequest(err)
		}
		config = storagePoolClusterConfigForEtag(config)
	}

	// Validate the ETag
	etag := []interface{}{dbInfo.Name, dbInfo.Driver, config}

	err = util.EtagCheck(r, etag)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	// Config stacking
	if req.Config == nil {
		req.Config = map[string]string{}
	}

	for k, v := range dbInfo.Config {
		_, ok := req.Config[k]
		if !ok {
			req.Config[k] = v
		}
	}

	// Validate the configuration
	err = storagePoolValidateConfig(poolName, dbInfo.Driver, req.Config, dbInfo.Config)
	if err != nil {
		return response.BadRequest(err)
	}

	config = req.Config
	if clustered {
		// For clustered requests, we need to complement the request's config
		// with our node-specific values.
		config = storagePoolClusterFillWithNodeConfig(dbInfo.Config, config)
	}

	// Notify the other nodes, unless this is itself a notification.
	if clustered && !isClusterNotification(r) {
		cert := d.endpoints.NetworkCert()
		notifier, err := cluster.NewNotifier(d.State(), cert, cluster.NotifyAll)
		if err != nil {
			return response.SmartError(err)
		}
		err = notifier(func(client lxd.InstanceServer) error {
			return client.UpdateStoragePool(poolName, req, r.Header.Get("If-Match"))
		})
		if err != nil {
			return response.SmartError(err)
		}
	}

	withDB := !isClusterNotification(r)
	err = storagePoolUpdate(d.State(), poolName, req.Description, config, withDB)
	if err != nil {
		return response.InternalError(err)
	}

	return response.EmptySyncResponse
}

// This helper makes sure that, when clustered, we're not changing
// node-specific values.
//
// POSSIBLY TODO: for now we don't have any node-specific values that can be
// modified. If we ever get some, we'll need to extend the PUT/PATCH APIs to
// accept a targetNode query parameter.
func storagePoolValidateClusterConfig(reqConfig map[string]string) error {
	for key := range reqConfig {
		if shared.StringInSlice(key, db.StoragePoolNodeConfigKeys) {
			return fmt.Errorf("node-specific config key %s can't be changed", key)
		}
	}
	return nil
}

// This helper deletes any node-specific values from the config object, since
// they should not be part of the calculated etag.
func storagePoolClusterConfigForEtag(dbConfig map[string]string) map[string]string {
	config := util.CopyConfig(dbConfig)
	for _, key := range db.StoragePoolNodeConfigKeys {
		delete(config, key)
	}
	return config
}

// This helper complements a PUT/PATCH request config with node-specific value,
// as taken from the db.
func storagePoolClusterFillWithNodeConfig(dbConfig, reqConfig map[string]string) map[string]string {
	config := util.CopyConfig(reqConfig)
	for _, key := range db.StoragePoolNodeConfigKeys {
		config[key] = dbConfig[key]
	}
	return config
}

// /1.0/storage-pools/{name}
// Delete storage pool.
func storagePoolDelete(d *Daemon, r *http.Request) response.Response {
	poolName := mux.Vars(r)["name"]

	poolID, err := d.cluster.StoragePoolGetID(poolName)
	if err != nil {
		return response.NotFound(err)
	}

	// If this is not an internal cluster request, check if the storage
	// pool has any volumes associated with it, if so error out.
	if !isClusterNotification(r) {
		response := storagePoolDeleteCheckPreconditions(d.cluster, poolName, poolID)
		if response != nil {
			return response
		}
	}

	// Check if the pool is pending, if so we just need to delete it from
	// the database.
	_, dbPool, err := d.cluster.StoragePoolGet(poolName)
	if err != nil {
		return response.SmartError(err)
	}

	if dbPool.Status == "Pending" {
		_, err := d.cluster.StoragePoolDelete(poolName)
		if err != nil {
			return response.SmartError(err)
		}
		return response.EmptySyncResponse
	}

	volumeNames, err := d.cluster.StoragePoolVolumesGetNames(poolID)
	if err != nil {
		return response.InternalError(err)
	}

	pool, err := storagePools.GetPoolByName(d.State(), poolName)
	if err != storageDrivers.ErrUnknownDriver {
		if err != nil {
			return response.InternalError(err)
		}

		// Only delete images if locally stored or running on initial member.
		if !isClusterNotification(r) || !pool.Driver().Info().Remote {
			for _, volume := range volumeNames {
				_, imgInfo, err := d.cluster.ImageGet("default", volume, false, false)
				if err != nil {
					return response.InternalError(err)
				}

				err = doDeleteImageFromPool(d.State(), imgInfo.Fingerprint, poolName)
				if err != nil {
					return response.InternalError(err)
				}
			}
		}

		err = pool.Delete(isClusterNotification(r), nil)
		if err != nil {
			return response.InternalError(err)
		}
	} else {
		s, err := storagePoolInit(d.State(), poolName)
		if err != nil {
			return response.InternalError(err)
		}

		// If this is a notification for a ceph pool deletion, we don't want to
		// actually delete the pool, since that will be done by the node that
		// notified us. We just need to delete the local mountpoint.
		if s, ok := s.(*storageCeph); ok && isClusterNotification(r) {
			// Delete the mountpoint for the storage pool.
			poolMntPoint := storagePools.GetStoragePoolMountPoint(s.pool.Name)
			if shared.PathExists(poolMntPoint) {
				err := os.RemoveAll(poolMntPoint)
				if err != nil {
					return response.SmartError(err)
				}
			}

			return response.EmptySyncResponse
		}

		for _, volume := range volumeNames {
			_, imgInfo, err := d.cluster.ImageGet("default", volume, false, false)
			if err != nil {
				return response.InternalError(err)
			}

			err = doDeleteImageFromPool(d.State(), imgInfo.Fingerprint, poolName)
			if err != nil {
				return response.InternalError(err)
			}
		}

		err = s.StoragePoolDelete()
		if err != nil {
			return response.InternalError(err)
		}
	}

	// If this is a cluster notification, we're done, any database work
	// will be done by the node that is originally serving the request.
	if isClusterNotification(r) {
		return response.EmptySyncResponse
	}

	// If we are clustered, also notify all other nodes, if any.
	clustered, err := cluster.Enabled(d.db)
	if err != nil {
		return response.SmartError(err)
	}
	if clustered {
		notifier, err := cluster.NewNotifier(d.State(), d.endpoints.NetworkCert(), cluster.NotifyAll)
		if err != nil {
			return response.SmartError(err)
		}
		err = notifier(func(client lxd.InstanceServer) error {
			_, _, err := client.GetServer()
			if err != nil {
				return err
			}
			return client.DeleteStoragePool(poolName)
		})
		if err != nil {
			return response.SmartError(err)
		}
	}

	err = dbStoragePoolDeleteAndUpdateCache(d.cluster, poolName)
	if err != nil {
		return response.SmartError(err)
	}

	return response.EmptySyncResponse
}

func storagePoolDeleteCheckPreconditions(cluster *db.Cluster, poolName string, poolID int64) response.Response {
	volumeNames, err := cluster.StoragePoolVolumesGetNames(poolID)
	if err != nil {
		return response.InternalError(err)
	}

	if len(volumeNames) > 0 {
		volumes, err := cluster.StoragePoolVolumesGet("default", poolID, supportedVolumeTypes)
		if err != nil {
			return response.InternalError(err)
		}

		for _, volume := range volumes {
			if volume.Type != "image" {
				return response.BadRequest(fmt.Errorf("storage pool \"%s\" has volumes attached to it", poolName))
			}
		}
	}

	// Check if the storage pool is still referenced in any profiles.
	profiles, err := profilesUsingPoolGetNames(cluster, poolName)
	if err != nil {
		return response.SmartError(err)
	}

	if len(profiles) > 0 {
		return response.BadRequest(fmt.Errorf("Storage pool \"%s\" has profiles using it:\n%s", poolName, strings.Join(profiles, "\n")))
	}

	return nil
}
