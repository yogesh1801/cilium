// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package experimental

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"sort"

	"github.com/cilium/hive/cell"
	"github.com/cilium/statedb"
	"github.com/cilium/statedb/reconciler"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/cilium/cilium/pkg/bpf"
	cmtypes "github.com/cilium/cilium/pkg/clustermesh/types"
	"github.com/cilium/cilium/pkg/loadbalancer"
	"github.com/cilium/cilium/pkg/maps/lbmap"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/u8proto"
)

// ReconcilerCell reconciles the load-balancing state with the BPF maps.
var ReconcilerCell = cell.Module(
	"reconciler",
	"Reconciles load-balancing state with BPF maps",

	cell.Provide(
		newBPFOps,
	),
	cell.Invoke(
		registerBPFReconciler,
	),
)

func registerBPFReconciler(p reconciler.Params, cfg Config, ops *bpfOps, w *Writer) error {
	if !w.IsEnabled() {
		return nil
	}
	_, err := reconciler.Register(
		p,
		w.fes,

		(*Frontend).Clone,
		(*Frontend).setStatus,
		(*Frontend).getStatus,
		ops,
		nil,

		reconciler.WithRetry(
			cfg.RetryBackoffMin,
			cfg.RetryBackoffMax,
		),
	)
	return err
}

type bpfOps struct {
	log    *slog.Logger
	lbmaps lbmaps

	serviceIDAlloc     idAllocator
	restoredServiceIDs sets.Set[loadbalancer.ID]
	backendIDAlloc     idAllocator
	restoredBackendIDs sets.Set[loadbalancer.BackendID]

	backendStates     map[loadbalancer.L3n4Addr]backendState
	backendReferences map[loadbalancer.L3n4Addr]sets.Set[loadbalancer.L3n4Addr]

	// nodePortAddrs are the last used NodePort addresses for a given NodePort
	// (or HostPort) ervice (by port).
	nodePortAddrs map[uint16][]netip.Addr
}

type backendState struct {
	addr     loadbalancer.L3n4Addr
	revision statedb.Revision
	refCount int
	id       loadbalancer.BackendID
}

func newBPFOps(lc cell.Lifecycle, log *slog.Logger, cfg Config, lbmaps lbmaps) *bpfOps {
	if !cfg.EnableExperimentalLB {
		return nil
	}
	ops := &bpfOps{
		serviceIDAlloc:     newIDAllocator(firstFreeServiceID, maxSetOfServiceID),
		restoredServiceIDs: sets.New[loadbalancer.ID](),
		backendIDAlloc:     newIDAllocator(firstFreeBackendID, maxSetOfBackendID),
		restoredBackendIDs: sets.New[loadbalancer.BackendID](),
		log:                log,
		backendStates:      map[loadbalancer.L3n4Addr]backendState{},
		backendReferences:  map[loadbalancer.L3n4Addr]sets.Set[loadbalancer.L3n4Addr]{},
		nodePortAddrs:      map[uint16][]netip.Addr{},
		lbmaps:             lbmaps,
	}
	lc.Append(cell.Hook{OnStart: ops.start})
	return ops
}

func (ops *bpfOps) start(_ cell.HookContext) error {
	// Restore the ID allocations from the BPF maps in order to reuse
	// them and thus avoiding traffic disruptions.
	err := ops.lbmaps.DumpService(func(key lbmap.ServiceKey, value lbmap.ServiceValue) {
		id := loadbalancer.ID(value.GetRevNat())
		ops.serviceIDAlloc.addID(svcKeyToAddr(key), id)
		ops.restoredServiceIDs.Insert(id)
	})
	if err != nil {
		return fmt.Errorf("restore service ids: %w", err)
	}

	err = ops.lbmaps.DumpBackend(func(key lbmap.BackendKey, value lbmap.BackendValue) {
		ops.backendIDAlloc.addID(beValueToAddr(value), loadbalancer.ID(key.GetID()))
		ops.restoredBackendIDs.Insert(key.GetID())
	})
	if err != nil {
		return fmt.Errorf("restore backend ids: %w", err)
	}

	return nil
}

func svcKeyToAddr(svcKey lbmap.ServiceKey) loadbalancer.L3n4Addr {
	feIP := svcKey.GetAddress()
	feAddrCluster := cmtypes.MustAddrClusterFromIP(feIP)
	feL3n4Addr := loadbalancer.NewL3n4Addr(loadbalancer.TCP /* FIXME */, feAddrCluster, svcKey.GetPort(), svcKey.GetScope())
	return *feL3n4Addr
}

func beValueToAddr(beValue lbmap.BackendValue) loadbalancer.L3n4Addr {
	feIP := beValue.GetAddress()
	feAddrCluster := cmtypes.MustAddrClusterFromIP(feIP)
	feL3n4Addr := loadbalancer.NewL3n4Addr(loadbalancer.TCP /* FIXME */, feAddrCluster, beValue.GetPort(), 0)
	return *feL3n4Addr
}

// Delete implements reconciler.Operations.
func (ops *bpfOps) Delete(_ context.Context, _ statedb.ReadTxn, fe *Frontend) error {
	if err := ops.deleteFrontend(fe); err != nil {
		ops.log.Warn("Deleting frontend failed, retrying", "error", err)
		return err
	}

	if fe.Type == loadbalancer.SVCTypeNodePort ||
		fe.Type == loadbalancer.SVCTypeHostPort && fe.Address.AddrCluster.IsUnspecified() {

		addrs, ok := ops.nodePortAddrs[fe.Address.Port]
		if ok {
			for _, addr := range addrs {
				fe = fe.Clone()
				fe.Address.AddrCluster = cmtypes.AddrClusterFrom(addr, 0)
				if err := ops.deleteFrontend(fe); err != nil {
					ops.log.Warn("Deleting frontend failed, retrying", "error", err)
					return err
				}
			}
			delete(ops.nodePortAddrs, fe.Address.Port)
		} else {
			ops.log.Warn("no nodePortAddrs", "port", fe.Address.Port)
		}
	}
	return nil
}

func (ops *bpfOps) deleteFrontend(fe *Frontend) error {
	feID, err := ops.serviceIDAlloc.lookupLocalID(fe.Address)
	if err != nil {
		// Since no ID was found we can assume this frontend was never reconciled.
		return nil
	}

	ops.log.Info("Delete frontend", "id", feID, "address", fe.Address)

	// Clean up any potential affinity match entries. We do this regardless of
	// whether or not SessionAffinity is enabled as it might've been toggled by
	// the user. Could optimize this by holding some more state if needed.
	for addr := range ops.backendReferences[fe.Address] {
		err := ops.deleteAffinityMatch(feID, ops.backendStates[addr].id)
		if err != nil {
			return fmt.Errorf("delete affinity match %d: %w", feID, err)
		}
	}

	for _, orphanState := range ops.orphanBackends(fe.Address, nil) {
		ops.log.Info("Delete orphan backend", "address", orphanState.addr)
		if err := ops.deleteBackend(orphanState.addr.IsIPv6(), orphanState.id); err != nil {
			return fmt.Errorf("delete backend %d: %w", orphanState.id, err)
		}
		ops.releaseBackend(orphanState.id, orphanState.addr)
	}

	var svcKey lbmap.ServiceKey
	var revNatKey lbmap.RevNatKey

	ip := fe.Address.AddrCluster.AsNetIP()
	if fe.Address.IsIPv6() {
		svcKey = lbmap.NewService6Key(ip, fe.Address.Port, u8proto.ANY, fe.Address.Scope, 0)
		revNatKey = lbmap.NewRevNat6Key(uint16(feID))
	} else {
		svcKey = lbmap.NewService4Key(ip, fe.Address.Port, u8proto.ANY, fe.Address.Scope, 0)
		revNatKey = lbmap.NewRevNat4Key(uint16(feID))
	}

	// Delete all slots including master.
	numBackends := len(ops.backendReferences[fe.Address])
	for i := 0; i <= numBackends; i++ {
		svcKey.SetBackendSlot(i)
		ops.log.Info("Delete service slot", "id", feID, "address", fe.Address, "slot", i)
		err := ops.lbmaps.DeleteService(svcKey.ToNetwork())
		if err != nil {
			return fmt.Errorf("delete from services map: %w", err)
		}
	}

	err = ops.lbmaps.DeleteRevNat(revNatKey.ToNetwork())
	if err != nil {
		return fmt.Errorf("delete reverse nat %d: %w", feID, err)
	}

	// Decrease the backend reference counts and drop state associated with the frontend.
	ops.updateBackendRefCounts(fe.Address, nil)
	delete(ops.backendReferences, fe.Address)
	ops.serviceIDAlloc.deleteLocalID(feID)

	return nil
}

func (ops *bpfOps) pruneServiceMaps() error {
	toDelete := []lbmap.ServiceKey{}
	svcCB := func(key bpf.MapKey, value bpf.MapValue) {
		svcKey := key.(lbmap.ServiceKey).ToHost()
		ac, ok := cmtypes.AddrClusterFromIP(svcKey.GetAddress())
		if !ok {
			ops.log.Warn("Prune: bad address in service key", "key", key)
			return
		}
		addr := loadbalancer.L3n4Addr{
			AddrCluster: ac,
			L4Addr:      loadbalancer.L4Addr{Protocol: loadbalancer.TCP /* FIXME */, Port: svcKey.GetPort()},
			Scope:       svcKey.GetScope(),
		}
		if _, ok := ops.backendReferences[addr]; !ok {
			toDelete = append(toDelete, svcKey)
		}
	}
	lbmap.Service4MapV2.DumpWithCallback(svcCB)
	lbmap.Service6MapV2.DumpWithCallback(svcCB)

	for _, key := range toDelete {
		if err := key.MapDelete(); err != nil {
			ops.log.Warn("Failed to delete from service map", "error", err)
		}
	}
	return nil
}

func (ops *bpfOps) pruneBackendMaps() error {
	toDelete := []lbmap.BackendKey{}
	beCB := func(key bpf.MapKey, value bpf.MapValue) {
		beKey := key.(lbmap.BackendKey)
		beValue := value.(lbmap.BackendValue).ToHost()
		if _, ok := ops.backendStates[beValueToAddr(beValue)]; !ok {
			ops.log.Info("pruneBackendMaps: deleting", "id", beKey.GetID(), "addr", beValueToAddr(beValue))
			toDelete = append(toDelete, beKey)
		}
	}
	lbmap.Backend4MapV3.DumpWithCallback(beCB)
	lbmap.Backend6MapV3.DumpWithCallback(beCB)
	for _, key := range toDelete {
		if err := key.Map().Delete(key); err != nil {
			ops.log.Warn("Failed to delete from backend map", "error", err)
		}
	}
	return nil
}

func (ops *bpfOps) pruneRestoredIDs() error {
	for id := range ops.restoredServiceIDs {
		if addr := ops.serviceIDAlloc.entitiesID[id]; addr != nil {
			if _, found := ops.backendReferences[addr.L3n4Addr]; !found {
				// This ID was restored but no frontend appeared to claim it. Free it.
				ops.serviceIDAlloc.deleteLocalID(id)
			}
		}
	}
	for id := range ops.restoredBackendIDs {
		if addr := ops.backendIDAlloc.entitiesID[loadbalancer.ID(id)]; addr != nil {
			if _, found := ops.backendStates[addr.L3n4Addr]; !found {
				// This ID was restored but no frontend appeared to claim it. Free it.
				ops.backendIDAlloc.deleteLocalID(loadbalancer.ID(id))
			}
		}
	}

	ops.restoredServiceIDs.Clear()
	ops.restoredBackendIDs.Clear()

	return nil
}

func (ops *bpfOps) pruneRevNat() error {
	toDelete := []lbmap.RevNatKey{}
	cb := func(key lbmap.RevNatKey, value lbmap.RevNatValue) {
		revNatKey := key.ToHost()
		if _, ok := ops.serviceIDAlloc.entitiesID[loadbalancer.ID(revNatKey.GetKey())]; !ok {
			ops.log.Info("pruneRevNat: deleting", "id", revNatKey.GetKey())
			toDelete = append(toDelete, revNatKey)
		}
	}
	err := ops.lbmaps.DumpRevNat(cb)
	if err != nil {
		return err
	}
	for _, key := range toDelete {
		err := ops.lbmaps.DeleteRevNat(key.ToNetwork())
		if err != nil {
			ops.log.Warn("Failed to delete from reverse nat map", "error", err)
		}
	}
	return nil
}

// Prune implements reconciler.Operations.
func (ops *bpfOps) Prune(_ context.Context, _ statedb.ReadTxn, _ statedb.Iterator[*Frontend]) error {
	return errors.Join(
		ops.pruneRestoredIDs(),
		ops.pruneServiceMaps(),
		ops.pruneBackendMaps(),
		ops.pruneRevNat(),
		// TODO rest of the maps.
	)
}

// Update implements reconciler.Operations.
func (ops *bpfOps) Update(_ context.Context, _ statedb.ReadTxn, fe *Frontend) error {
	if err := ops.updateFrontend(fe); err != nil {
		ops.log.Warn("Updating frontend failed, retrying", "error", err)
		return err
	}

	if fe.Type == loadbalancer.SVCTypeNodePort ||
		fe.Type == loadbalancer.SVCTypeHostPort && fe.Address.AddrCluster.IsUnspecified() {
		// For NodePort create entries for each node address.
		// For HostPort only create them if the address was not specified (HostIP is unset).
		// TODO: HostPort loopback?
		// TODO: When the nodeport addresses change trigger a full refresh by marking everything as
		// pending?

		old := sets.New(ops.nodePortAddrs[fe.Address.Port]...)
		for _, addr := range fe.nodePortAddrs {
			if fe.Address.IsIPv6() != addr.Is6() {
				continue
			}
			fe = fe.Clone()
			fe.Address.AddrCluster = cmtypes.AddrClusterFrom(addr, 0)
			if err := ops.updateFrontend(fe); err != nil {
				ops.log.Warn("Updating frontend failed, retrying", "error", err)
				return err
			}
			old.Delete(addr)
		}

		// Delete orphan NodePort/HostPort frontends
		for addr := range old {
			if fe.Address.IsIPv6() != addr.Is6() {
				continue
			}
			fe = fe.Clone()
			fe.Address.AddrCluster = cmtypes.AddrClusterFrom(addr, 0)
			if err := ops.deleteFrontend(fe); err != nil {
				ops.log.Warn("Deleting orphan frontend failed, retrying", "error", err)
				return err
			}
		}
		ops.nodePortAddrs[fe.Address.Port] = fe.nodePortAddrs
	}

	return nil
}

func (ops *bpfOps) updateFrontend(fe *Frontend) error {
	// WARNING: This method must be idempotent. Any updates to state must happen only after
	// the operations that depend on the state have been performed. If this invariant is not
	// followed then we may leak data due to not retrying a failed operation.

	// Assign/lookup an identifier for the service. May fail if we have run out of IDs.
	// The Frontend.ID field is purely for debugging purposes.
	feID, err := ops.serviceIDAlloc.acquireLocalID(fe.Address, 0)
	if err != nil {
		return fmt.Errorf("failed to allocate id: %w", err)
	}

	var svcKey lbmap.ServiceKey
	var svcVal lbmap.ServiceValue

	ip := fe.Address.AddrCluster.AsNetIP()
	if fe.Address.IsIPv6() {
		svcKey = lbmap.NewService6Key(ip, fe.Address.Port, u8proto.ANY, fe.Address.Scope, 0)
		svcVal = &lbmap.Service6Value{}
	} else {
		svcKey = lbmap.NewService4Key(ip, fe.Address.Port, u8proto.ANY, fe.Address.Scope, 0)
		svcVal = &lbmap.Service4Value{}
	}

	// isRoutable denotes whether this service can be accessed from outside the cluster.
	isRoutable := !svcKey.IsSurrogate() &&
		(fe.Type != loadbalancer.SVCTypeClusterIP || option.Config.ExternalClusterIP)
	svc := fe.Service()
	flag := loadbalancer.NewSvcFlag(&loadbalancer.SvcFlagParam{
		SvcType:          fe.Type,
		SvcExtLocal:      svc.ExtTrafficPolicy == loadbalancer.SVCTrafficPolicyLocal,
		SvcIntLocal:      svc.IntTrafficPolicy == loadbalancer.SVCTrafficPolicyLocal,
		SvcNatPolicy:     svc.NatPolicy,
		SessionAffinity:  svc.SessionAffinity,
		IsRoutable:       isRoutable,
		L7LoadBalancer:   svc.L7ProxyPort != 0,
		LoopbackHostport: svc.LoopbackHostPort,

		// TODO:
		//CheckSourceRange: checkSourceRange,
	})
	svcVal.SetFlags(flag.UInt16())
	svcVal.SetRevNat(int(feID))

	// Gather backends for the service
	orderedBackends := sortedBackends(fe.Backends)

	ops.log.Info("orderedBackends", "backends", orderedBackends)

	// Clean up any orphan backends to make room for new backends
	backendAddrs := sets.New[loadbalancer.L3n4Addr]()
	for _, be := range orderedBackends {
		backendAddrs.Insert(be.L3n4Addr)
	}

	for _, orphanState := range ops.orphanBackends(fe.Address, backendAddrs) {
		ops.log.Info("Delete orphan backend", "address", orphanState.addr)
		if err := ops.deleteBackend(orphanState.addr.IsIPv6(), orphanState.id); err != nil {
			return fmt.Errorf("delete backend: %w", err)
		}
		if err := ops.deleteAffinityMatch(feID, orphanState.id); err != nil {
			return fmt.Errorf("delete affinity match: %w", err)
		}
		ops.releaseBackend(orphanState.id, orphanState.addr)
	}

	// Update backends that are new or changed.
	for i, be := range orderedBackends {
		var beID loadbalancer.BackendID
		if s, ok := ops.backendStates[be.L3n4Addr]; ok && s.id != 0 {
			beID = s.id
		} else {
			acquiredID, err := ops.backendIDAlloc.acquireLocalID(be.L3n4Addr, 0)
			if err != nil {
				return err
			}
			beID = loadbalancer.BackendID(acquiredID)
		}

		if ops.needsUpdate(be.L3n4Addr, be.Revision) {
			ops.log.Info("Update backend", "backend", be, "id", beID, "addr", be.L3n4Addr)
			if err := ops.upsertBackend(beID, be.Backend); err != nil {
				return fmt.Errorf("upsert backend: %w", err)
			}

			ops.updateBackendRevision(beID, be.L3n4Addr, be.Revision)
		}

		// Update the service slot for the backend. We do this regardless
		// if the backend entry is up-to-date since the backend slot order might've
		// changed.
		// Since backends are iterated in the order of their state with active first
		// the slot ids here are sequential.
		if be.State == loadbalancer.BackendStateActive {
			ops.log.Info("Update service slot", "id", beID, "slot", i+1, "backendID", beID)

			svcVal.SetBackendID(beID)
			svcVal.SetRevNat(int(feID))
			svcKey.SetBackendSlot(i + 1)
			if err := ops.upsertService(svcKey, svcVal); err != nil {
				return fmt.Errorf("upsert service: %w", err)
			}
		}

		// TODO: Most likely we'll just need to keep some state on the reconciled SessionAffinity
		// state to avoid the extra syscalls when session affinity is not enabled.
		// For now we update these regardless so that we handle properly the SessionAffinity being
		// flipped on and then off.
		if svc.SessionAffinity && be.State == loadbalancer.BackendStateActive {
			if err := ops.upsertAffinityMatch(feID, beID); err != nil {
				return fmt.Errorf("upsert affinity match: %w", err)
			}
		} else {
			// SessionAffinity either disabled or backend not active, no matter which
			// clean up any affinity match that might exist.
			if err := ops.deleteAffinityMatch(feID, beID); err != nil {
				return fmt.Errorf("delete affinity match: %w", err)
			}
		}
	}

	// Backends updated successfully, we can now update the references.
	numPreviousBackends := len(ops.backendReferences[fe.Address])

	// Update RevNat
	ops.log.Info("Update RevNat", "id", feID, "address", fe.Address)
	if err := ops.upsertRevNat(feID, svcKey, svcVal); err != nil {
		return fmt.Errorf("upsert reverse nat: %w", err)
	}

	ops.log.Info("Update master service", "id", feID)
	numActiveBackends := numActive(orderedBackends)
	if err := ops.upsertMaster(svcKey, svcVal, fe, numActiveBackends); err != nil {
		return fmt.Errorf("upsert service master: %w", err)
	}

	ops.log.Info("Cleanup service slots", "id", feID, "active", numActiveBackends, "previous", numPreviousBackends)
	if err := ops.cleanupSlots(svcKey, numPreviousBackends, numActiveBackends); err != nil {
		return fmt.Errorf("cleanup service slots: %w", err)
	}

	// Finally update the new references. This makes sure any failures reconciling the service slots
	// above can be retried and entries are not leaked.
	ops.updateReferences(fe.Address, backendAddrs)

	return nil
}

func (ops *bpfOps) upsertService(svcKey lbmap.ServiceKey, svcVal lbmap.ServiceValue) error {
	var err error
	svcKey = svcKey.ToNetwork()
	svcVal = svcVal.ToNetwork()

	err = ops.lbmaps.UpdateService(svcKey, svcVal)
	if errors.Is(err, unix.E2BIG) {
		return fmt.Errorf("Unable to update service entry %+v => %+v: "+
			"Unable to update element for LB bpf map: "+
			"You can resize it with the flag \"--%s\". "+
			"The resizing might break existing connections to services",
			svcKey, svcVal, option.LBMapEntriesName)
	}
	return err
}

func (ops *bpfOps) upsertMaster(svcKey lbmap.ServiceKey, svcVal lbmap.ServiceValue, fe *Frontend, activeBackends int) error {
	svcKey.SetBackendSlot(0)
	svcVal.SetCount(activeBackends)
	svcVal.SetBackendID(0)

	svc := fe.Service()

	// Set the SessionAffinity/L7ProxyPort. These re-use the "backend ID".
	if svc.SessionAffinity {
		svcVal.SetSessionAffinityTimeoutSec(uint32(svc.SessionAffinityTimeout.Seconds()))
	}
	if svc.L7ProxyPort != 0 {
		svcVal.SetL7LBProxyPort(svc.L7ProxyPort)
	}
	return ops.upsertService(svcKey, svcVal)
}

func (ops *bpfOps) cleanupSlots(svcKey lbmap.ServiceKey, oldCount, newCount int) error {
	for i := newCount; i < oldCount; i++ {
		svcKey.SetBackendSlot(i + 1)
		_, err := svcKey.Map().SilentDelete(svcKey.ToNetwork())
		if err != nil {
			return fmt.Errorf("cleanup service slot %q: %w", svcKey.String(), err)
		}
	}
	return nil
}

func (ops *bpfOps) upsertBackend(id loadbalancer.BackendID, be *Backend) (err error) {
	var lbbe lbmap.Backend
	if be.AddrCluster.Is6() {
		lbbe, err = lbmap.NewBackend6V3(id, be.AddrCluster, be.Port, u8proto.ANY,
			be.State, be.ZoneID)
		if err != nil {
			return err
		}
	} else {
		lbbe, err = lbmap.NewBackend4V3(id, be.AddrCluster, be.Port, u8proto.ANY,
			be.State, be.ZoneID)
		if err != nil {
			return err
		}
	}
	return ops.lbmaps.UpdateBackend(
		lbbe.GetKey(),
		lbbe.GetValue().ToNetwork(),
	)
}

func (ops *bpfOps) deleteBackend(ipv6 bool, id loadbalancer.BackendID) error {
	var key lbmap.BackendKey
	if ipv6 {
		key = lbmap.NewBackend6KeyV3(id)
	} else {
		key = lbmap.NewBackend4KeyV3(id)
	}
	_, err := key.Map().SilentDelete(key)
	if err != nil {
		return fmt.Errorf("delete backend %d: %w", id, err)
	}
	return nil
}

func (ops *bpfOps) upsertAffinityMatch(id loadbalancer.ID, beID loadbalancer.BackendID) error {
	if !option.Config.EnableSessionAffinity {
		return nil
	}

	key := &lbmap.AffinityMatchKey{
		BackendID: beID,
		RevNATID:  uint16(id),
	}
	var value lbmap.AffinityMatchValue
	ops.log.Info("upsertAffinityMatch", "key", key)
	return lbmap.AffinityMatchMap.Update(key.ToNetwork(), &value)
}

func (ops *bpfOps) deleteAffinityMatch(id loadbalancer.ID, beID loadbalancer.BackendID) error {
	if !option.Config.EnableSessionAffinity {
		return nil
	}

	key := &lbmap.AffinityMatchKey{
		BackendID: beID,
		RevNATID:  uint16(id),
	}
	ops.log.Info("deleteAffinityMatch", "serviceID", id, "backendID", beID)
	_, err := lbmap.AffinityMatchMap.SilentDelete(key.ToNetwork())
	return err
}

func (ops *bpfOps) upsertRevNat(id loadbalancer.ID, svcKey lbmap.ServiceKey, svcVal lbmap.ServiceValue) error {
	zeroValue := svcVal.New().(lbmap.ServiceValue)
	zeroValue.SetRevNat(int(id))
	revNATKey := zeroValue.RevNatKey()
	revNATValue := svcKey.RevNatValue()

	if revNATKey.GetKey() == 0 {
		return fmt.Errorf("invalid RevNat ID (0)")
	}
	ops.log.Info("upsertRevNat", "key", revNATKey, "value", revNATValue)

	err := ops.lbmaps.UpdateRevNat(revNATKey.ToNetwork(), revNATValue.ToNetwork())
	if err != nil {
		return fmt.Errorf("Unable to update reverse NAT %+v => %+v: %w", revNATKey, revNATValue, err)
	}
	return nil

}

var _ reconciler.Operations[*Frontend] = &bpfOps{}

func (ops *bpfOps) updateBackendRefCounts(frontend loadbalancer.L3n4Addr, backends sets.Set[loadbalancer.L3n4Addr]) {
	newRefs := backends.Clone()

	// Decrease reference counts of backends that are no longer referenced
	// by this frontend.
	if oldRefs, ok := ops.backendReferences[frontend]; ok {
		for addr := range oldRefs {
			if newRefs.Has(addr) {
				newRefs.Delete(addr)
				continue
			}
			s, ok := ops.backendStates[addr]
			if ok && s.refCount > 1 {
				s.refCount--
				ops.backendStates[addr] = s
			}
		}
	}

	// Increase the reference counts of backends that are newly
	// referenced.
	for addr := range newRefs {
		s := ops.backendStates[addr]
		s.addr = addr
		s.refCount++
		ops.backendStates[addr] = s
	}
}

func (ops *bpfOps) updateReferences(frontend loadbalancer.L3n4Addr, backends sets.Set[loadbalancer.L3n4Addr]) {
	ops.updateBackendRefCounts(frontend, backends)
	ops.backendReferences[frontend] = backends
}

func (ops *bpfOps) orphanBackends(frontend loadbalancer.L3n4Addr, backends sets.Set[loadbalancer.L3n4Addr]) (orphans []backendState) {
	if oldRefs, ok := ops.backendReferences[frontend]; ok {
		for addr := range oldRefs {
			if backends.Has(addr) {
				continue
			}
			// If there is only one reference to this backend then it's from this frontend and
			// since it's not part of the new set it has become an orphan.
			if state, ok := ops.backendStates[addr]; ok && state.refCount <= 1 {
				orphans = append(orphans, state)
			}
		}
	}
	return orphans
}

// checkBackend returns true if the backend should be updated.
func (ops *bpfOps) needsUpdate(addr loadbalancer.L3n4Addr, rev statedb.Revision) bool {
	return rev > ops.backendStates[addr].revision
}

func (ops *bpfOps) updateBackendRevision(id loadbalancer.BackendID, addr loadbalancer.L3n4Addr, rev statedb.Revision) {
	s := ops.backendStates[addr]
	s.id = id
	s.revision = rev
	ops.backendStates[addr] = s
}

// releaseBackend releases the backends information and the ID when it has been deleted
// successfully.
func (ops *bpfOps) releaseBackend(id loadbalancer.BackendID, addr loadbalancer.L3n4Addr) {
	delete(ops.backendStates, addr)
	ops.backendIDAlloc.deleteLocalID(loadbalancer.ID(id))
}

// sortedBackends sorts the backends in-place with the following sort order:
// - State (active first)
// - Address
// - Port
//
// Backends are sorted to deterministically to keep the order stable in BPF maps
// when updating.
func sortedBackends(bes []BackendWithRevision) []BackendWithRevision {
	sort.Slice(bes, func(i, j int) bool {
		a, b := bes[i], bes[j]
		switch {
		case a.State < b.State:
			return true
		case a.State > b.State:
			return false
		default:
			switch a.L3n4Addr.AddrCluster.Addr().Compare(b.L3n4Addr.AddrCluster.Addr()) {
			case -1:
				return true
			case 0:
				return a.L3n4Addr.Port < b.L3n4Addr.Port
			default:
				return false
			}
		}
	})
	return bes
}

func numActive(bes []BackendWithRevision) int {
	for i, be := range bes {
		if be.State != loadbalancer.BackendStateActive {
			return i
		}
	}
	return len(bes)
}

// idAllocator contains an internal state of the ID allocator.
type idAllocator struct {
	// entitiesID is a map of all entities indexed by service or backend ID
	entitiesID map[loadbalancer.ID]*loadbalancer.L3n4AddrID

	// entities is a map of all entities indexed by L3n4Addr.StringID()
	entities map[string]loadbalancer.ID

	// nextID is the next ID to attempt to allocate
	nextID loadbalancer.ID

	// maxID is the maximum ID available for allocation
	maxID loadbalancer.ID

	// initNextID is the initial nextID
	initNextID loadbalancer.ID

	// initMaxID is the initial maxID
	initMaxID loadbalancer.ID
}

const (
	// firstFreeServiceID is the first ID for which the services should be assigned.
	firstFreeServiceID = loadbalancer.ID(1)

	// maxSetOfServiceID is maximum number of set of service IDs that can be stored
	// in the kvstore or the local ID allocator.
	maxSetOfServiceID = loadbalancer.ID(0xFFFF)

	// firstFreeBackendID is the first ID for which the backend should be assigned.
	// BPF datapath assumes that backend_id cannot be 0.
	firstFreeBackendID = loadbalancer.ID(1)

	// maxSetOfBackendID is maximum number of set of backendIDs IDs that can be
	// stored in the local ID allocator.
	maxSetOfBackendID = loadbalancer.ID(0xFFFFFFFF)
)

func newIDAllocator(nextID loadbalancer.ID, maxID loadbalancer.ID) idAllocator {
	return idAllocator{
		entitiesID: map[loadbalancer.ID]*loadbalancer.L3n4AddrID{},
		entities:   map[string]loadbalancer.ID{},
		nextID:     nextID,
		maxID:      maxID,
		initNextID: nextID,
		initMaxID:  maxID,
	}
}

func (alloc *idAllocator) addID(svc loadbalancer.L3n4Addr, id loadbalancer.ID) loadbalancer.ID {
	svcID := newID(svc, id)
	alloc.entitiesID[id] = svcID
	alloc.entities[svc.StringID()] = id
	return id
}

func (alloc *idAllocator) acquireLocalID(svc loadbalancer.L3n4Addr, desiredID loadbalancer.ID) (loadbalancer.ID, error) {
	if svcID, ok := alloc.entities[svc.StringID()]; ok {
		if svc, ok := alloc.entitiesID[svcID]; ok {
			return svc.ID, nil
		}
	}

	if desiredID != 0 {
		foundSVC, ok := alloc.entitiesID[desiredID]
		if !ok {
			if desiredID >= alloc.nextID {
				// We don't set nextID to desiredID+1 here, as we don't want to
				// duplicate the logic which deals with the rollover. Next
				// invocation of acquireLocalID(..., 0) will fix the nextID.
				alloc.nextID = desiredID
			}
			return alloc.addID(svc, desiredID), nil
		}
		return 0, fmt.Errorf("Service ID %d is already registered to %q",
			desiredID, foundSVC)
	}

	startingID := alloc.nextID
	rollover := false
	for {
		if alloc.nextID == startingID && rollover {
			break
		} else if alloc.nextID == alloc.maxID {
			alloc.nextID = alloc.initNextID
			rollover = true
		}

		if _, ok := alloc.entitiesID[alloc.nextID]; !ok {
			svcID := alloc.addID(svc, alloc.nextID)
			alloc.nextID++
			return svcID, nil
		}

		alloc.nextID++
	}

	return 0, fmt.Errorf("no service ID available")
}

func (alloc *idAllocator) deleteLocalID(id loadbalancer.ID) {
	if svc, ok := alloc.entitiesID[id]; ok {
		delete(alloc.entitiesID, id)
		delete(alloc.entities, svc.StringID())
	}
}

func (alloc *idAllocator) lookupLocalID(svc loadbalancer.L3n4Addr) (loadbalancer.ID, error) {
	if svcID, ok := alloc.entities[svc.StringID()]; ok {
		return svcID, nil
	}

	return 0, fmt.Errorf("ID not found")
}

func newID(svc loadbalancer.L3n4Addr, id loadbalancer.ID) *loadbalancer.L3n4AddrID {
	return &loadbalancer.L3n4AddrID{
		L3n4Addr: svc,
		ID:       loadbalancer.ID(id),
	}
}
