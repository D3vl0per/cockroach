// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package roachprod

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/cockroach/pkg/roachprod/install"
	"github.com/cockroachdb/errors"
)

// StartTenant starts nodes on a cluster in "tenant" mode (each node is a SQL
// instance). A tenant cluster needs an existing, running host cluster. The
// tenant metadata is created on the host cluster if it doesn't exist already.
//
// The host and tenant can use the same underlying cluster, as long as different
// subsets of nodes are selected (e.g. "local:1,2" and "local:3,4").
func StartTenant(
	tenantCluster string,
	hostCluster string,
	startOpts install.StartOpts,
	clusterSettingsOpts ...install.ClusterSettingOption,
) error {
	tc, err := newCluster(tenantCluster, clusterSettingsOpts...)
	if err != nil {
		return err
	}

	// TODO(radu): do we need separate clusterSettingsOpts for the host cluster?
	hc, err := newCluster(hostCluster, clusterSettingsOpts...)
	if err != nil {
		return err
	}

	if tc.Name == hc.Name {
		// We allow using the same cluster, but the node sets must be disjoint.
		for _, n1 := range tc.Nodes {
			for _, n2 := range hc.Nodes {
				if n1 == n2 {
					return errors.Errorf("host and tenant nodes must be disjoint")
				}
			}
		}
	}

	if tc.Secure {
		// TODO(radu): implement secure mode.
		return errors.Errorf("secure mode not implemented for tenants yet")
	}

	startOpts.Target = install.StartTenantSQL
	if startOpts.TenantID < 2 {
		return errors.Errorf("invalid tenant ID %d (must be 2 or higher)", startOpts.TenantID)
	}

	// Create tenant, if necessary. We need to run this SQL against a single host,
	// so temporarily restrict the target nodes to 1.
	saveNodes := hc.Nodes
	hc.Nodes = hc.Nodes[:1]
	fmt.Printf("Creating tenant metadata\n")
	if err := hc.RunSQL([]string{
		`-e`,
		fmt.Sprintf(createTenantIfNotExistsQuery, startOpts.TenantID),
	}); err != nil {
		return err
	}
	hc.Nodes = saveNodes

	var kvAddrs []string
	for _, node := range hc.Nodes {
		kvAddrs = append(kvAddrs, fmt.Sprintf("%s:%d", hc.Host(node), hc.NodePort(node)))
	}
	startOpts.KVAddrs = strings.Join(kvAddrs, ",")
	return tc.Start(startOpts)
}

// createTenantIfNotExistsQuery is used to initialize the tenant metadata, if
// it's not initialized already. We set up the tenant with a lot of initial RUs
// so that we don't encounter throttling by default.
const createTenantIfNotExistsQuery = `
SELECT
  CASE (SELECT 1 FROM system.tenants WHERE id = %[1]d) IS NULL
  WHEN true
  THEN (
    crdb_internal.create_tenant(%[1]d),
    crdb_internal.update_tenant_resource_limits(%[1]d, 1000000000, 10000, 0, now(), 0)
  )::STRING
  ELSE 'already exists'
  END;`
