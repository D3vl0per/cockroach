// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package spec

import (
	"fmt"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/roachprod/vm"
	"github.com/cockroachdb/cockroach/pkg/roachprod/vm/aws"
	"github.com/cockroachdb/cockroach/pkg/roachprod/vm/azure"
	"github.com/cockroachdb/cockroach/pkg/roachprod/vm/gce"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
)

type fileSystemType int

const (
	// Since ext4 is the default of 0, it isn't being
	// used anywhere in the code. Therefore, it isn't
	// added as a const here since it is unused, and
	// leads to a lint error.

	// Zfs file system.
	Zfs fileSystemType = 1
)

// ClusterSpec represents a test's description of what its cluster needs to
// look like. It becomes part of a clusterConfig when the cluster is created.
type ClusterSpec struct {
	Cloud        string
	InstanceType string // auto-chosen if left empty
	NodeCount    int
	// CPUs is the number of CPUs per node.
	CPUs           int
	SSDs           int
	RAID0          bool
	VolumeSize     int
	PreferLocalSSD bool
	Zones          string
	Geo            bool
	Lifetime       time.Duration
	ReusePolicy    clusterReusePolicy

	// FileSystem determines the underlying FileSystem
	// to be used. The default is ext4.
	FileSystem fileSystemType

	RandomlyUseZfs bool
}

// MakeClusterSpec makes a ClusterSpec.
func MakeClusterSpec(cloud string, instanceType string, nodeCount int, opts ...Option) ClusterSpec {
	spec := ClusterSpec{Cloud: cloud, InstanceType: instanceType, NodeCount: nodeCount}
	defaultOpts := []Option{CPU(4), nodeLifetimeOption(12 * time.Hour), ReuseAny()}
	for _, o := range append(defaultOpts, opts...) {
		o.apply(&spec)
	}
	return spec
}

// ClustersCompatible returns true if the clusters are compatible, i.e. the test
// asking for s2 can reuse s1.
func ClustersCompatible(s1, s2 ClusterSpec) bool {
	s1.Lifetime = 0
	s2.Lifetime = 0
	return s1 == s2
}

// String implements fmt.Stringer.
func (s ClusterSpec) String() string {
	str := fmt.Sprintf("n%dcpu%d", s.NodeCount, s.CPUs)
	if s.Geo {
		str += "-Geo"
	}
	return str
}

// checks if an AWS machine supports SSD volumes
func awsMachineSupportsSSD(machineType string) bool {
	typeAndSize := strings.Split(machineType, ".")
	if len(typeAndSize) == 2 {
		// All SSD machine types that we use end in 'd or begins with i3 (e.g. i3, i3en).
		return strings.HasPrefix(typeAndSize[0], "i3") || strings.HasSuffix(typeAndSize[0], "d")
	}
	return false
}

func getAWSOpts(machineType string, zones []string, localSSD bool) vm.ProviderOpts {
	opts := aws.DefaultProviderOpts()
	if localSSD {
		opts.SSDMachineType = machineType
	} else {
		opts.MachineType = machineType
	}
	if len(zones) != 0 {
		opts.CreateZones = zones
	}
	return opts
}

func getGCEOpts(
	machineType string, zones []string, volumeSize, localSSDCount int, localSSD bool, RAID0 bool,
) vm.ProviderOpts {
	opts := gce.DefaultProviderOpts()
	opts.MachineType = machineType
	if volumeSize != 0 {
		opts.PDVolumeSize = volumeSize
	}
	if len(zones) != 0 {
		opts.Zones = zones
	}
	opts.SSDCount = localSSDCount
	if localSSD && localSSDCount > 0 {
		// NB: As the default behavior for _roachprod_ (at least in AWS/GCP) is
		// to mount multiple disks as a single store using a RAID 0 array, we
		// must explicitly ask for multiple stores to be enabled, _unless_ the
		// test has explicitly asked for RAID0.
		opts.UseMultipleDisks = !RAID0
	}
	return opts
}

func getAzureOpts(machineType string, zones []string) vm.ProviderOpts {
	opts := azure.DefaultProviderOpts()
	opts.MachineType = machineType
	if len(zones) != 0 {
		opts.Locations = zones
	}
	return opts
}

// RoachprodOpts returns the opts to use when calling `roachprod.Create()`
// in order to create the cluster described in the spec.
func (s *ClusterSpec) RoachprodOpts(
	clusterName string, useIOBarrier bool,
) (vm.CreateOpts, vm.ProviderOpts, error) {

	createVMOpts := vm.DefaultCreateOpts()
	createVMOpts.ClusterName = clusterName
	if s.Lifetime != 0 {
		createVMOpts.Lifetime = s.Lifetime
	}
	switch s.Cloud {
	case Local:
		createVMOpts.VMProviders = []string{s.Cloud}
		// remaining opts are not applicable to local clusters
		return createVMOpts, nil, nil
	case AWS, GCE, Azure:
		createVMOpts.VMProviders = []string{s.Cloud}
	default:
		return vm.CreateOpts{}, nil, errors.Errorf("unsupported cloud %v", s.Cloud)
	}

	if s.Cloud != GCE {
		if s.VolumeSize != 0 {
			return vm.CreateOpts{}, nil, errors.Errorf("specifying volume size is not yet supported on %s", s.Cloud)
		}
		if s.SSDs != 0 {
			return vm.CreateOpts{}, nil, errors.Errorf("specifying SSD count is not yet supported on %s", s.Cloud)
		}
	}

	createVMOpts.GeoDistributed = s.Geo
	machineType := s.InstanceType
	if s.CPUs != 0 {
		// Default to the user-supplied machine type, if any.
		// Otherwise, pick based on requested CPU count.
		if len(machineType) == 0 {
			// If no machine type was specified, choose one
			// based on the cloud and CPU count.
			switch s.Cloud {
			case AWS:
				machineType = AWSMachineType(s.CPUs)
			case GCE:
				machineType = GCEMachineType(s.CPUs)
			case Azure:
				machineType = AzureMachineType(s.CPUs)
			}
		}

		// Local SSD can only be requested
		// - if configured to prefer doing so,
		// - if no particular volume size is requested, and,
		// - on AWS, if the machine type supports it.
		if s.PreferLocalSSD && s.VolumeSize == 0 && (s.Cloud != AWS || awsMachineSupportsSSD(machineType)) {
			// Ensure SSD count is at least 1 if UseLocalSSD is true.
			if s.SSDs == 0 {
				s.SSDs = 1
			}
			createVMOpts.SSDOpts.UseLocalSSD = true
			createVMOpts.SSDOpts.NoExt4Barrier = !useIOBarrier
		} else {
			createVMOpts.SSDOpts.UseLocalSSD = false
		}
	}

	if s.FileSystem == Zfs {
		if s.Cloud != GCE {
			return vm.CreateOpts{}, nil, errors.Errorf(
				"node creation with zfs file system not yet supported on %s", s.Cloud,
			)
		}
		createVMOpts.SSDOpts.FileSystem = vm.Zfs
	} else if s.RandomlyUseZfs && s.Cloud == GCE {
		rng, _ := randutil.NewPseudoRand()
		if rng.Float64() <= 0.2 {
			createVMOpts.SSDOpts.FileSystem = vm.Zfs
		}
	}
	var zones []string
	if s.Zones != "" {
		zones = strings.Split(s.Zones, ",")
		if !s.Geo {
			zones = zones[:1]
		}
	}

	var providerOpts vm.ProviderOpts
	switch s.Cloud {
	case AWS:
		providerOpts = getAWSOpts(machineType, zones, createVMOpts.SSDOpts.UseLocalSSD)
	case GCE:
		providerOpts = getGCEOpts(machineType, zones, s.VolumeSize, s.SSDs, createVMOpts.SSDOpts.UseLocalSSD, s.RAID0)
	case Azure:
		providerOpts = getAzureOpts(machineType, zones)
	}

	return createVMOpts, providerOpts, nil
}

// Expiration is the lifetime of the cluster. It may be destroyed after
// the expiration has passed.
func (s *ClusterSpec) Expiration() time.Time {
	l := s.Lifetime
	if l == 0 {
		l = 12 * time.Hour
	}
	return timeutil.Now().Add(l)
}
