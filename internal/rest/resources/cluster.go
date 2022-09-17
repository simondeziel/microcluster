package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	dqliteClient "github.com/canonical/go-dqlite/client"
	"github.com/gorilla/mux"
	"github.com/lxc/lxd/lxd/response"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
	"golang.org/x/sys/unix"

	"github.com/canonical/microcluster/cluster"
	"github.com/canonical/microcluster/internal/db"
	"github.com/canonical/microcluster/internal/rest/access"
	"github.com/canonical/microcluster/internal/rest/client"
	internalTypes "github.com/canonical/microcluster/internal/rest/types"
	"github.com/canonical/microcluster/rest/types"

	"github.com/canonical/microcluster/internal/state"
	"github.com/canonical/microcluster/internal/trust"
	"github.com/canonical/microcluster/rest"
)

var clusterCmd = rest.Endpoint{
	Path: "cluster",

	Post: rest.EndpointAction{Handler: clusterPost, AllowUntrusted: true},
	Get:  rest.EndpointAction{Handler: clusterGet, AccessHandler: access.AllowAuthenticated},
}

var clusterMemberCmd = rest.Endpoint{
	Path: "cluster/{name}",

	Put:    rest.EndpointAction{Handler: clusterMemberPut, AccessHandler: access.AllowAuthenticated},
	Delete: rest.EndpointAction{Handler: clusterMemberDelete, AccessHandler: access.AllowAuthenticated},
}

func clusterPost(state *state.State, r *http.Request) response.Response {
	req := internalTypes.ClusterMember{}

	// Parse the request.
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		return response.BadRequest(err)
	}

	// Set a 5 second timeout in case dqlite locks up.
	ctx, cancel := context.WithTimeout(state.Context, time.Second*5)
	defer cancel()

	leaderClient, err := state.Database.Leader(ctx)
	if err != nil {
		return response.SmartError(err)
	}

	leaderInfo, err := leaderClient.Leader(ctx)
	if err != nil {
		return response.SmartError(err)
	}

	// Check if any of the remote's addresses are currently in use.
	existingRemote := state.Remotes().RemoteByAddress(req.Address)
	if existingRemote != nil {
		return response.SmartError(fmt.Errorf("Remote with address %q exists", req.Address.String()))
	}

	newRemote := trust.Remote{
		Name:        req.Name,
		Address:     req.Address,
		Certificate: req.Certificate,
	}

	// Forward request to leader.
	if leaderInfo.Address != state.Address.URL.Host {
		client, err := state.Leader()
		if err != nil {
			return response.SmartError(err)
		}

		tokenResponse, err := client.AddClusterMember(state.Context, req)
		if err != nil {
			return response.SmartError(err)
		}

		// If we are not the leader, just add the cluster member to our local store for authentication.
		err = state.Remotes().Add(state.OS.TrustDir, newRemote)
		if err != nil {
			return response.SmartError(err)
		}

		return response.SyncResponse(true, tokenResponse)
	}

	err = state.Database.Transaction(state.Context, func(ctx context.Context, tx *db.Tx) error {
		dbClusterMember := cluster.InternalClusterMember{
			Name:        req.Name,
			Address:     req.Address.String(),
			Certificate: req.Certificate.String(),
			Schema:      req.SchemaVersion,
			Heartbeat:   time.Time{},
			Role:        cluster.Pending,
		}

		record, err := cluster.GetInternalTokenRecord(ctx, tx, req.Secret)
		if err != nil {
			return err
		}

		_, err = cluster.CreateInternalClusterMember(ctx, tx, dbClusterMember)
		if err != nil {
			return err
		}

		return cluster.DeleteInternalTokenRecord(ctx, tx, record.Name)
	})
	if err != nil {
		return response.SmartError(err)
	}

	remotes := state.Remotes()
	clusterMembers := make([]internalTypes.ClusterMemberLocal, 0, remotes.Count())
	for _, clusterMember := range remotes.RemotesByName() {
		clusterMember := internalTypes.ClusterMemberLocal{
			Name:        clusterMember.Name,
			Address:     clusterMember.Address,
			Certificate: clusterMember.Certificate,
		}

		clusterMembers = append(clusterMembers, clusterMember)
	}

	clusterCert, err := state.ClusterCert().PublicKeyX509()
	if err != nil {
		return response.SmartError(err)
	}

	tokenResponse := internalTypes.TokenResponse{
		ClusterCert: types.X509Certificate{Certificate: clusterCert},
		ClusterKey:  string(state.ClusterCert().PrivateKey()),

		ClusterMembers: clusterMembers,
	}

	// Add the cluster member to our local store for authentication.
	err = state.Remotes().Add(state.OS.TrustDir, newRemote)
	if err != nil {
		return response.SmartError(err)
	}

	return response.SyncResponse(true, tokenResponse)
}

func clusterGet(state *state.State, r *http.Request) response.Response {
	var apiClusterMembers []internalTypes.ClusterMember
	err := state.Database.Transaction(state.Context, func(ctx context.Context, tx *db.Tx) error {
		clusterMembers, err := cluster.GetInternalClusterMembers(ctx, tx)
		if err != nil {
			return err
		}

		apiClusterMembers = make([]internalTypes.ClusterMember, 0, len(clusterMembers))
		for _, clusterMember := range clusterMembers {
			apiClusterMember, err := clusterMember.ToAPI()
			if err != nil {
				return err
			}

			apiClusterMembers = append(apiClusterMembers, *apiClusterMember)
		}

		return nil
	})
	if err != nil {
		return response.SmartError(fmt.Errorf("Failed to get cluster members: %w", err))
	}

	clusterCert, err := state.ClusterCert().PublicKeyX509()
	if err != nil {
		return response.SmartError(err)
	}

	// Send a small request to each node to ensure they are reachable.
	for i, clusterMember := range apiClusterMembers {
		addr := api.NewURL().Scheme("https").Host(clusterMember.Address.String())
		d, err := client.New(*addr, state.ServerCert(), clusterCert, false)
		if err != nil {
			return response.SmartError(fmt.Errorf("Failed to create HTTPS client for cluster member with address %q: %w", addr.String(), err))
		}

		err = d.CheckReady(state.Context)
		if err == nil {
			apiClusterMembers[i].Status = internalTypes.MemberOnline
		} else {
			logger.Warnf("Failed to get status of cluster member with address %q: %v", addr.String(), err)
		}
	}

	return response.SyncResponse(true, apiClusterMembers)
}

// clusterDisableMu is used to prevent the daemon process from being replaced/stopped during removal from the
// cluster until such time as the request that initiated the removal has finished. This allows for self removal
// from the cluster when not the leader.
var clusterDisableMu sync.Mutex

// Re-execs the daemon of the cluster member with a fresh state.
func clusterMemberPut(state *state.State, r *http.Request) response.Response {
	err := state.Database.Stop()
	if err != nil {
		return response.SmartError(fmt.Errorf("Failed shutting down database: %w", err))
	}

	err = os.RemoveAll(state.OS.StateDir)
	if err != nil {
		return response.SmartError(fmt.Errorf("Failed to remove the state directory: %w", err))
	}

	go func() {
		<-r.Context().Done() // Wait until request has finished.

		// Wait until we can acquire the lock. This way if another request is holding the lock we won't
		// replace/stop the LXD daemon until that request has finished.
		clusterDisableMu.Lock()
		defer clusterDisableMu.Unlock()
		execPath, err := os.Readlink("/proc/self/exe")
		if err != nil {
			execPath = "bad-exec-path"
		}

		// The execPath from /proc/self/exe can end with " (deleted)" if the lxd binary has been removed/changed
		// since the lxd process was started, strip this so that we only return a valid path.
		logger.Info("Restarting daemon following removal from cluster")
		execPath = strings.TrimSuffix(execPath, " (deleted)")
		err = unix.Exec(execPath, os.Args, os.Environ())
		if err != nil {
			logger.Error("Failed restarting daemon", logger.Ctx{"err": err})
		}
	}()

	return response.ManualResponse(func(w http.ResponseWriter) error {
		err := response.EmptySyncResponse.Render(w)
		if err != nil {
			return err
		}

		// Send the response before replacing the LXD daemon process.
		f, ok := w.(http.Flusher)
		if ok {
			f.Flush()
		} else {
			return fmt.Errorf("http.ResponseWriter is not type http.Flusher")
		}

		return nil
	})
}

// clusterMemberDelete Removes a cluster member from dqlite and re-execs its daemon.
func clusterMemberDelete(state *state.State, r *http.Request) response.Response {
	name, err := url.PathUnescape(mux.Vars(r)["name"])
	if err != nil {
		return response.SmartError(err)
	}

	allRemotes := state.Remotes().RemotesByName()
	remote, ok := allRemotes[name]
	if !ok {
		return response.SmartError(fmt.Errorf("No remote exists with the given name %q", name))
	}

	ctx, cancel := context.WithTimeout(state.Context, time.Second*30)
	defer cancel()

	leader, err := state.Database.Leader(ctx)
	if err != nil {
		return response.SmartError(err)
	}

	leaderInfo, err := leader.Leader(ctx)
	if err != nil {
		return response.SmartError(err)
	}

	// If we are not the leader, just update our trust store.
	if leaderInfo.Address != state.Address.URL.Host {
		if allRemotes[name].Address.String() == state.Address.URL.Host {
			// If the member being removed is ourselves and we are not the leader, then lock the
			// clusterPutDisableMu before we forward the request to the leader, so that when the leader
			// goes on to request clusterPutDisable back to ourselves it won't be actioned until we
			// have returned this request back to the original client.
			clusterDisableMu.Lock()
			logger.Info("Acquired cluster self removal lock", logger.Ctx{"member": name})

			go func() {
				<-r.Context().Done() // Wait until request is finished.

				logger.Info("Releasing cluster self removal lock", logger.Ctx{"member": name})
				clusterDisableMu.Unlock()
			}()
		}

		client, err := state.Leader()
		if err != nil {
			return response.SmartError(err)
		}

		err = client.DeleteClusterMember(state.Context, name)
		if err != nil {
			return response.SmartError(err)
		}

		newRemotes := []internalTypes.ClusterMember{}
		for _, remote := range allRemotes {
			if remote.Name != name {
				clusterMember := internalTypes.ClusterMemberLocal{Name: remote.Name, Address: remote.Address, Certificate: remote.Certificate}
				newRemotes = append(newRemotes, internalTypes.ClusterMember{ClusterMemberLocal: clusterMember})
			}
		}

		err = state.Remotes().Replace(state.OS.TrustDir, newRemotes...)
		if err != nil {
			return response.SmartError(err)
		}

		return response.ManualResponse(func(w http.ResponseWriter) error {
			err := response.EmptySyncResponse.Render(w)
			if err != nil {
				return err
			}

			// Send the response before replacing the LXD daemon process.
			f, ok := w.(http.Flusher)
			if ok {
				f.Flush()
			} else {
				return fmt.Errorf("http.ResponseWriter is not type http.Flusher")
			}

			return nil
		})
	}

	info, err := leader.Cluster(state.Context)
	if err != nil {
		return response.SmartError(err)
	}

	index := -1
	for i, node := range info {
		if node.Address == remote.Address.String() {
			index = i
			break
		}
	}

	if index < 0 {
		return response.SmartError(fmt.Errorf("No dqlite cluster member exists with the given name %q", name))
	}

	localClient, err := client.New(state.OS.ControlSocket(), nil, nil, false)
	if err != nil {
		return response.SmartError(err)
	}

	clusterMembers, err := localClient.GetClusterMembers(state.Context)
	if err != nil {
		return response.SmartError(err)
	}

	numPending := 0
	for _, clusterMember := range clusterMembers {
		if clusterMember.Role == string(cluster.Pending) {
			numPending++
		}
	}

	if len(clusterMembers)-numPending < 2 {
		return response.SmartError(fmt.Errorf("Cannot remove cluster members, there are no remaining non-pending members"))
	}

	if len(info) < 2 {
		return response.SmartError(fmt.Errorf("Cannot leave a cluster with %d members", len(info)))
	}

	if len(info) == 2 && allRemotes[name].Address.String() == leaderInfo.Address {
		for _, node := range info {
			if node.Address != leaderInfo.Address {
				err = leader.Assign(ctx, node.ID, dqliteClient.Voter)
				if err != nil {
					return response.SmartError(err)
				}
			}
		}
	}

	// Remove the cluster member from the database.
	err = state.Database.Transaction(state.Context, func(ctx context.Context, tx *db.Tx) error {
		return cluster.DeleteInternalClusterMember(ctx, tx, info[index].Address)
	})
	if err != nil {
		return response.SmartError(err)
	}

	// Remove the node from dqlite.
	err = leader.Remove(state.Context, info[index].ID)
	if err != nil {
		return response.SmartError(err)
	}

	// Reset the state of the removed node.
	if allRemotes[name].Address.String() == state.Address.URL.Host {
		return clusterMemberPut(state, r)
	} else {

		newRemotes := []internalTypes.ClusterMember{}
		for _, remote := range allRemotes {
			if remote.Name != name {
				clusterMember := internalTypes.ClusterMemberLocal{Name: remote.Name, Address: remote.Address, Certificate: remote.Certificate}
				newRemotes = append(newRemotes, internalTypes.ClusterMember{ClusterMemberLocal: clusterMember})
			}
		}

		// Remove the cluster member from the leader's trust store.
		err = state.Remotes().Replace(state.OS.TrustDir, newRemotes...)
		if err != nil {
			return response.SmartError(err)
		}

		remote := allRemotes[name]
		publicKey, err := state.ClusterCert().PublicKeyX509()
		if err != nil {
			return response.SmartError(err)
		}

		client, err := client.New(remote.URL(), state.ServerCert(), publicKey, false)
		if err != nil {
			return response.SmartError(err)
		}

		err = client.ResetClusterMember(state.Context, name)
		if err != nil {
			return response.SmartError(err)
		}
	}

	return response.EmptySyncResponse
}