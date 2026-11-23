package provider

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	serverv1 "github.com/relay-sh/relay-go/gen/relay/server/v1"
)

func (f *fakeServer) updateDistribution(merged, in *serverv1.DatabaseSpec) error {
	if merged.GetEngine() == enginePostgreSQL {
		return updatePostgreSQLDistribution(merged, in)
	}
	if (merged.GetDistribution() == nil) != (in.GetDistribution() == nil) {
		return status.Error(codes.FailedPrecondition, "changing the distribution mode of a valkey database is not supported")
	}
	merged.Distribution = f.canonicalRegions(in.GetDistribution())
	return nil
}

func (f *fakeServer) canonicalRegions(d *serverv1.Distribution) *serverv1.Distribution {
	if d == nil {
		return nil
	}
	out := &serverv1.Distribution{Mode: d.GetMode(), Clouds: map[string]*serverv1.CloudRegions{}}
	for cloud, c := range d.GetClouds() {
		for region, p := range c.GetRegions() {
			for _, known := range f.providers {
				if strings.EqualFold(known.GetCloud(), cloud) && strings.EqualFold(known.GetRegion(), region) {
					cloud, region = known.GetCloud(), known.GetRegion()
				}
			}
			if out.Clouds[cloud] == nil {
				out.Clouds[cloud] = &serverv1.CloudRegions{Regions: map[string]*serverv1.RegionPlacement{}}
			}
			out.Clouds[cloud].Regions[region] = p
		}
	}
	return out
}

func (f *fakeServer) gatePromotion(old, next *serverv1.Distribution) error {
	if to := primaryRegionOf(next); f.unsyncedStandby && to != primaryRegionOf(old) {
		return status.Errorf(codes.FailedPrecondition, "the standby region %s cannot be promoted yet: its head has not synchronized with the primary region yet (replication offset 0); retry once it has synchronized, or force the promotion to accept losing whatever it has not received", to)
	}
	return nil
}

func updatePostgreSQLDistribution(merged, in *serverv1.DatabaseSpec) error {
	old, next := merged.GetDistribution(), in.GetDistribution()
	standby := old.GetMode() == distributionStandby && next.GetMode() == distributionStandby
	if before, after := regionKeys(old), regionKeys(next); !standby && !slices.Equal(before, after) {
		mode := distributionLocal
		if len(before) > 0 {
			mode = old.GetMode()
		}
		return status.Errorf(codes.InvalidArgument, "changing the regions or cluster bindings of a %s database is not supported yet", mode)
	}
	merged.Distribution = next
	return nil
}

type fakeRegion struct {
	cloud, region string
	placement     *serverv1.RegionPlacement
}

func regionEntries(d *serverv1.Distribution) map[string]fakeRegion {
	out := map[string]fakeRegion{}
	for cloud, c := range d.GetClouds() {
		for region, p := range c.GetRegions() {
			out[regionKey(cloud, region)] = fakeRegion{cloud: cloud, region: region, placement: p}
		}
	}
	return out
}

func regionsMissingFrom(have, other map[string]fakeRegion) []fakeRegion {
	var out []fakeRegion
	for _, key := range slices.Sorted(maps.Keys(have)) {
		if _, ok := other[key]; !ok {
			out = append(out, have[key])
		}
	}
	return out
}

func (f *fakeServer) gateDistribution(old, next *serverv1.DatabaseSpec) error {
	if err := f.standbyRegionChange(old, next); err != nil {
		return err
	}
	return f.gatePromotion(old.GetDistribution(), next.GetDistribution())
}

func (f *fakeServer) standbyRegionChange(old, next *serverv1.DatabaseSpec) error {
	if old.GetDistribution().GetMode() != distributionStandby || next.GetDistribution().GetMode() != distributionStandby {
		return nil
	}
	before, after := regionEntries(old.GetDistribution()), regionEntries(next.GetDistribution())
	added, removed := regionsMissingFrom(after, before), regionsMissingFrom(before, after)
	if changes := len(added) + len(removed); changes == 0 {
		return nil
	} else if changes > 1 {
		return status.Errorf(codes.FailedPrecondition, "add or remove one region of a standby database per update, this update changes %d", changes)
	}
	expected := proto.Clone(old).(*serverv1.DatabaseSpec)
	var region fakeRegion
	if len(removed) == 1 {
		region = removed[0]
		if region.placement.GetPrimary() {
			return status.Errorf(codes.FailedPrecondition, "region %s/%s holds the primary marker, so it cannot be removed; promote another region by moving the marker first", region.cloud, region.region)
		}
		delete(expected.Distribution.Clouds[region.cloud].Regions, region.region)
		if len(expected.Distribution.Clouds[region.cloud].Regions) == 0 {
			delete(expected.Distribution.Clouds, region.cloud)
		}
	} else {
		region = added[0]
	}
	if !strings.EqualFold(primaryRegionOf(old.GetDistribution()), primaryRegionOf(next.GetDistribution())) {
		return status.Error(codes.FailedPrecondition, "the update moves the primary marker together with a region change; promote in one update and add or remove the region in another")
	}
	if len(added) == 1 {
		if expected.Distribution.Clouds[region.cloud] == nil {
			expected.Distribution.Clouds[region.cloud] = &serverv1.CloudRegions{Regions: map[string]*serverv1.RegionPlacement{}}
		}
		expected.Distribution.Clouds[region.cloud].Regions[region.region] = proto.Clone(region.placement).(*serverv1.RegionPlacement)
	}
	if !proto.Equal(next, expected) {
		return status.Error(codes.FailedPrecondition, "the update changes the regions together with other fields; send the region change alone and the other fields in a separate update")
	}
	if f.noTopologyOps {
		return status.Error(codes.Unimplemented, "adding or removing a region of a standby database runs as a topology operation: topology operations are not enabled on this deployment")
	}
	return nil
}

func regionKeys(d *serverv1.Distribution) []string {
	if d.GetMode() == "" || d.GetMode() == distributionLocal {
		return nil
	}
	var keys []string
	for cloud, c := range d.GetClouds() {
		for region := range c.GetRegions() {
			keys = append(keys, strings.ToLower(strings.TrimSpace(cloud))+"/"+strings.ToLower(strings.TrimSpace(region)))
		}
	}
	slices.Sort(keys)
	return keys
}

func rejectSpreadGroupMove(stored *serverv1.DatabaseSpec, next []*serverv1.NodeSpec) error {
	if stored.GetEngine() != enginePostgreSQL || len(regionKeys(stored.GetDistribution())) < 2 {
		return nil
	}
	for _, n := range next {
		for _, was := range stored.GetNodes() {
			if was.GetName() == n.GetName() && (!strings.EqualFold(was.GetCloud(), n.GetCloud()) || !strings.EqualFold(was.GetRegion(), n.GetRegion())) {
				return status.Error(codes.InvalidArgument, "moving a node group of a multi-region database is not supported yet")
			}
		}
	}
	return nil
}

func (f *fakeServer) rejectCrossClusterStandby(spec *serverv1.DatabaseSpec) error {
	if f.noStandbyMesh && spec.GetEngine() == enginePostgreSQL && spec.GetDistribution().GetMode() == distributionStandby {
		return status.Error(codes.FailedPrecondition, "standby across clusters is not supported yet: a standby region is bound to a different cluster than the primary region")
	}
	return nil
}

func validatePostgreSQLDistribution(spec *serverv1.DatabaseSpec) error {
	d := spec.GetDistribution()
	if spec.GetEngine() != enginePostgreSQL || d == nil {
		return nil
	}
	invalid := func(format string, args ...any) error {
		return status.Errorf(codes.InvalidArgument, "validate spec: "+format, args...)
	}
	owner := map[int32]string{}
	for _, cloud := range slices.Sorted(maps.Keys(d.GetClouds())) {
		regions := d.GetClouds()[cloud].GetRegions()
		for _, region := range slices.Sorted(maps.Keys(regions)) {
			p := regions[region].Priority
			switch {
			case p == nil:
				continue
			case d.GetMode() != distributionStretched:
				return invalid("distribution region %s/%s sets a priority, which only a %q distribution takes", cloud, region, distributionStretched)
			case *p < 1:
				return invalid("distribution region %s/%s priority must be >= 1, got %d", cloud, region, *p)
			case owner[*p] != "":
				return invalid("distribution regions %s and %s/%s share priority %d; priorities must be unique", owner[*p], cloud, region, *p)
			}
			owner[*p] = cloud + "/" + region
		}
	}
	if d.GetMode() == "" || d.GetMode() == distributionLocal {
		return nil
	}
	if d.GetMode() != distributionStandby && d.GetMode() != distributionStretched {
		return invalid("distribution.mode must be %q, %q or %q, got %q", distributionLocal, distributionStandby, distributionStretched, d.GetMode())
	}
	if d.GetMode() == distributionStretched && spec.GetDcs().GetType() != "etcd" {
		return invalid("distribution.mode %q requires dcs.type \"etcd\" (a Kubernetes-API DCS cannot span clusters)", distributionStretched)
	}
	regions, primaries := 0, 0
	for _, c := range d.GetClouds() {
		for _, p := range c.GetRegions() {
			regions++
			if p.GetPrimary() {
				primaries++
			}
			if p.Members != nil && (*p.Members < 0 || *p.Members == 0 && d.GetMode() != distributionStretched) {
				return invalid("distribution region members must be >= 1 when set (0 only for a stretched region hosting a read_only node group), got %d", *p.Members)
			}
		}
	}
	if regions < 2 {
		return invalid("distribution.mode %q requires at least 2 regions, got %d", d.GetMode(), regions)
	}
	if primaries != 1 {
		return invalid("distribution requires exactly one region marked primary, got %d", primaries)
	}
	for _, n := range spec.GetNodes() {
		if n.GetCloud() == "" || n.GetRegion() == "" {
			continue
		}
		if _, ok := d.GetClouds()[n.GetCloud()].GetRegions()[n.GetRegion()]; !ok {
			return invalid("node group %q is placed in %s/%s, which is not a region of the %s distribution; its members would never run there", n.GetName(), n.GetCloud(), n.GetRegion(), d.GetMode())
		}
	}
	if d.GetMode() == distributionStretched {
		return validateStretchedMembers(spec)
	}
	return nil
}

func validateStretchedMembers(spec *serverv1.DatabaseSpec) error {
	var sum, electable int32
	for cloud, c := range spec.GetDistribution().GetClouds() {
		for region, p := range c.GetRegions() {
			if p.Members == nil {
				return status.Errorf(codes.InvalidArgument, "validate spec: distribution.mode %q requires members on every region (%s/%s has none)", distributionStretched, cloud, region)
			}
			sum += *p.Members
		}
	}
	for _, n := range spec.GetNodes() {
		if n.GetRole() == roleElectable {
			electable += n.GetCount()
		}
	}
	if sum != electable {
		return status.Error(codes.InvalidArgument, fmt.Sprintf("validate spec: stretched distribution places %d members across regions, but the electable node groups hold %d", sum, electable))
	}
	return nil
}
