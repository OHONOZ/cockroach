// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package tests

import (
	"context"
	gosql "database/sql"
	"fmt"
	"time"

	"github.com/cockroachdb/cockroach/pkg/base"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/cluster"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/option"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/registry"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/spec"
	"github.com/cockroachdb/cockroach/pkg/cmd/roachtest/test"
	"github.com/cockroachdb/cockroach/pkg/roachprod/install"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/stretchr/testify/require"
)

func registerFailover(r registry.Registry) {
	for _, expirationLeases := range []bool{false, true} {
		expirationLeases := expirationLeases // pin loop variable
		var suffix string
		if expirationLeases {
			suffix = "/lease=expiration"
		}

		r.Add(registry.TestSpec{
			Name:    "failover/partial/lease-gateway" + suffix,
			Owner:   registry.OwnerKV,
			Timeout: 30 * time.Minute,
			Cluster: r.MakeClusterSpec(8, spec.CPU(4)),
			Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
				runFailoverPartialLeaseGateway(ctx, t, c, expirationLeases)
			},
		})

		r.Add(registry.TestSpec{
			Name:    "failover/partial/lease-leader" + suffix,
			Owner:   registry.OwnerKV,
			Timeout: 30 * time.Minute,
			Cluster: r.MakeClusterSpec(7, spec.CPU(4)),
			Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
				runFailoverPartialLeaseLeader(ctx, t, c, expirationLeases)
			},
		})

		r.Add(registry.TestSpec{
			Name:    "failover/partial/lease-liveness" + suffix,
			Owner:   registry.OwnerKV,
			Timeout: 30 * time.Minute,
			Cluster: r.MakeClusterSpec(8, spec.CPU(4)),
			Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
				runFailoverPartialLeaseLiveness(ctx, t, c, expirationLeases)
			},
		})

		for _, failureMode := range []failureMode{
			failureModeBlackhole,
			failureModeBlackholeRecv,
			failureModeBlackholeSend,
			failureModeCrash,
			failureModeDiskStall,
			failureModePause,
		} {
			failureMode := failureMode // pin loop variable
			makeSpec := func(nNodes, nCPU int) spec.ClusterSpec {
				s := r.MakeClusterSpec(nNodes, spec.CPU(nCPU))
				if failureMode == failureModeDiskStall {
					// Use PDs in an attempt to work around flakes encountered when using
					// SSDs. See #97968.
					s.PreferLocalSSD = false
				}
				return s
			}
			var postValidation registry.PostValidation = 0
			if failureMode == failureModeDiskStall {
				postValidation = registry.PostValidationNoDeadNodes
			}
			r.Add(registry.TestSpec{
				Name:                fmt.Sprintf("failover/non-system/%s%s", failureMode, suffix),
				Owner:               registry.OwnerKV,
				Timeout:             30 * time.Minute,
				SkipPostValidations: postValidation,
				Cluster:             makeSpec(7 /* nodes */, 4 /* cpus */),
				Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
					runFailoverNonSystem(ctx, t, c, failureMode, expirationLeases)
				},
			})
			r.Add(registry.TestSpec{
				Name:                fmt.Sprintf("failover/liveness/%s%s", failureMode, suffix),
				Owner:               registry.OwnerKV,
				Timeout:             30 * time.Minute,
				SkipPostValidations: postValidation,
				Cluster:             makeSpec(5 /* nodes */, 4 /* cpus */),
				Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
					runFailoverLiveness(ctx, t, c, failureMode, expirationLeases)
				},
			})
			r.Add(registry.TestSpec{
				Name:                fmt.Sprintf("failover/system-non-liveness/%s%s", failureMode, suffix),
				Owner:               registry.OwnerKV,
				Timeout:             30 * time.Minute,
				SkipPostValidations: postValidation,
				Cluster:             makeSpec(7 /* nodes */, 4 /* cpus */),
				Run: func(ctx context.Context, t test.Test, c cluster.Cluster) {
					runFailoverSystemNonLiveness(ctx, t, c, failureMode, expirationLeases)
				},
			})
		}
	}
}

// runFailoverPartialLeaseGateway tests a partial network partition between a
// SQL gateway and a user range leaseholder. These must be routed via other
// nodes to be able to serve the request.
//
// Cluster topology:
//
// n1-n3: system ranges and user ranges (2/5 replicas)
// n4-n5: user range leaseholders (2/5 replicas)
// n6-n7: SQL gateways and 1 user replica (1/5 replicas)
//
// 5 user range replicas will be placed on n2-n6, with leases on n4. A partial
// partition will be introduced between n4,n5 and n6,n7, both fully and
// individually. This corresponds to the case where we have three data centers
// with a broken network link between one pair. For example:
//
//	                        n1-n3 (2 replicas, liveness)
//	                          A
//	                        /   \
//	                       /     \
//	               n4-n5  B --x-- C  n6-n7  <---  n8 (workload)
//	(2 replicas, leases)             (1 replica, SQL gateways)
//
// Once follower routing is implemented, this tests the following scenarios:
//
// - Routes via followers in both A, B, and C when possible.
// - Skips follower replica on local node that can't reach leaseholder (n6).
// - Skips follower replica in C that can't reach leaseholder (n7 via n6).
// - Skips follower replica in B that's unreachable (n5).
//
// We run a kv50 workload on SQL gateways and collect pMax latency for graphing.
func runFailoverPartialLeaseGateway(
	ctx context.Context, t test.Test, c cluster.Cluster, expLeases bool,
) {
	require.Equal(t, 8, c.Spec().NodeCount)

	rng, _ := randutil.NewTestRand()

	// Create cluster.
	opts := option.DefaultStartOpts()
	settings := install.MakeClusterSettings()

	failer := makeFailer(t, c, failureModeBlackhole, opts, settings).(partialFailer)
	failer.Setup(ctx)
	defer failer.Cleanup(ctx)

	c.Put(ctx, t.Cockroach(), "./cockroach")
	c.Start(ctx, t.L(), opts, settings, c.Range(1, 7))

	conn := c.Conn(ctx, t.L(), 1)
	defer conn.Close()

	_, err := conn.ExecContext(ctx, `SET CLUSTER SETTING kv.expiration_leases_only.enabled = $1`,
		expLeases)
	require.NoError(t, err)

	// Place all ranges on n1-n3 to start with.
	configureAllZones(t, ctx, conn, zoneConfig{replicas: 3, onlyNodes: []int{1, 2, 3}})

	// Wait for upreplication.
	require.NoError(t, WaitFor3XReplication(ctx, t, conn))

	// Create the kv database with 5 replicas on n2-n6, and leases on n4.
	t.Status("creating workload database")
	_, err = conn.ExecContext(ctx, `CREATE DATABASE kv`)
	require.NoError(t, err)
	configureZone(t, ctx, conn, `DATABASE kv`, zoneConfig{
		replicas: 5, onlyNodes: []int{2, 3, 4, 5, 6}, leaseNode: 4})

	c.Run(ctx, c.Node(6), `./cockroach workload init kv --splits 1000 {pgurl:1}`)

	// Wait for the KV table to upreplicate.
	waitForUpreplication(t, ctx, conn, `database_name = 'kv'`, 5)

	// The replicate queue takes forever to move the ranges, so we do it
	// ourselves. Precreating the database/range and moving it to the correct
	// nodes first is not sufficient, since workload will spread the ranges across
	// all nodes regardless.
	relocateRanges(t, ctx, conn, `database_name = 'kv'`, []int{1, 7}, []int{2, 3, 4, 5, 6})
	relocateRanges(t, ctx, conn, `database_name != 'kv'`, []int{4, 5, 6, 7}, []int{1, 2, 3})
	relocateLeases(t, ctx, conn, `database_name = 'kv'`, 4)

	// Start workload on n8 using n6-n7 as gateways.
	t.Status("running workload")
	m := c.NewMonitor(ctx, c.Range(1, 7))
	m.Go(func(ctx context.Context) error {
		c.Run(ctx, c.Node(8), `./cockroach workload run kv --read-percent 50 `+
			`--duration 20m --concurrency 256 --max-rate 2048 --timeout 1m --tolerate-errors `+
			`--histograms=`+t.PerfArtifactsDir()+`/stats.json `+
			`{pgurl:6-7}`)
		return nil
	})

	// Start a worker to fail and recover partial partitions between n4,n5
	// (leases) and n6,n7 (gateways), both fully and individually, for 3 cycles.
	// Leases are only placed on n4.
	failer.Ready(ctx, m)
	m.Go(func(ctx context.Context) error {
		var raftCfg base.RaftConfig
		raftCfg.SetDefaults()

		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for i := 0; i < 3; i++ {
			testcases := []struct {
				nodes []int
				peers []int
			}{
				// Fully partition leases from gateways, must route via n1-n3. In
				// addition to n4 leaseholder being unreachable, follower on n5 is
				// unreachable, and follower replica on n6 can't reach leaseholder.
				{[]int{6, 7}, []int{4, 5}},
				// Partition n6 (gateway with local follower) from n4 (leaseholder).
				// Local follower replica can't reach leaseholder.
				{[]int{6}, []int{4}},
				// Partition n7 (gateway) from n4 (leaseholder).
				{[]int{7}, []int{4}},
			}
			for _, tc := range testcases {
				select {
				case <-ticker.C:
				case <-ctx.Done():
					return ctx.Err()
				}

				randTimer := time.After(randutil.RandDuration(rng, raftCfg.RangeLeaseRenewalDuration()))

				// Ranges and leases may occasionally escape their constraints. Move
				// them to where they should be.
				relocateRanges(t, ctx, conn, `database_name = 'kv'`, []int{1, 7}, []int{2, 3, 4, 5, 6})
				relocateRanges(t, ctx, conn, `database_name != 'kv'`, []int{4, 5, 6, 7}, []int{1, 2, 3})
				relocateLeases(t, ctx, conn, `database_name = 'kv'`, 4)

				// Randomly sleep up to the lease renewal interval, to vary the time
				// between the last lease renewal and the failure. We start the timer
				// before the range relocation above to run them concurrently.
				select {
				case <-randTimer:
				case <-ctx.Done():
				}

				for _, node := range tc.nodes {
					t.Status(fmt.Sprintf("failing n%d (blackhole lease/gateway)", node))
					failer.FailPartial(ctx, node, tc.peers)
				}

				select {
				case <-ticker.C:
				case <-ctx.Done():
					return ctx.Err()
				}

				for _, node := range tc.nodes {
					t.Status(fmt.Sprintf("recovering n%d (blackhole lease/gateway)", node))
					failer.Recover(ctx, node)
				}
			}
		}
		return nil
	})
	m.Wait()
}

// runFailoverLeaseLeader tests a partial network partition between leaseholders
// and Raft leaders. These will prevent the leaseholder from making Raft
// proposals, but it can still hold onto leases as long as it can heartbeat
// liveness.
//
// Cluster topology:
//
// n1-n3: system and liveness ranges, SQL gateway
// n4-n6: user ranges
//
// The cluster runs with COCKROACH_DISABLE_LEADER_FOLLOWS_LEASEHOLDER, which
// will place Raft leaders and leases independently of each other. We can then
// assume that some number of user ranges will randomly have split leader/lease,
// and simply create partial partitions between each of n4-n6 in sequence.
//
// We run a kv50 workload on SQL gateways and collect pMax latency for graphing.
func runFailoverPartialLeaseLeader(
	ctx context.Context, t test.Test, c cluster.Cluster, expLeases bool,
) {
	require.Equal(t, 7, c.Spec().NodeCount)

	rng, _ := randutil.NewTestRand()

	// Create cluster, disabling leader/leaseholder colocation. We only start
	// n1-n3, to precisely place system ranges, since we'll have to disable the
	// replicate queue shortly.
	opts := option.DefaultStartOpts()
	settings := install.MakeClusterSettings()
	settings.Env = append(settings.Env, "COCKROACH_DISABLE_LEADER_FOLLOWS_LEASEHOLDER=true")

	failer := makeFailer(t, c, failureModeBlackhole, opts, settings).(partialFailer)
	failer.Setup(ctx)
	defer failer.Cleanup(ctx)

	c.Put(ctx, t.Cockroach(), "./cockroach")
	c.Start(ctx, t.L(), opts, settings, c.Range(1, 3))

	conn := c.Conn(ctx, t.L(), 1)
	defer conn.Close()

	_, err := conn.ExecContext(ctx, `SET CLUSTER SETTING kv.expiration_leases_only.enabled = $1`,
		expLeases)
	require.NoError(t, err)

	// Place all ranges on n1-n3 to start with, and wait for upreplication.
	configureAllZones(t, ctx, conn, zoneConfig{replicas: 3, onlyNodes: []int{1, 2, 3}})
	require.NoError(t, WaitFor3XReplication(ctx, t, conn))

	// Disable the replicate queue. It can otherwise end up with stuck
	// overreplicated ranges during rebalancing, because downreplication requires
	// the Raft leader to be colocated with the leaseholder.
	_, err = conn.ExecContext(ctx, `SET CLUSTER SETTING kv.replicate_queue.enabled = false`)
	require.NoError(t, err)

	// Now that system ranges are properly placed on n1-n3, start n4-n6.
	c.Start(ctx, t.L(), opts, settings, c.Range(4, 6))

	// Create the kv database on n4-n6.
	t.Status("creating workload database")
	_, err = conn.ExecContext(ctx, `CREATE DATABASE kv`)
	require.NoError(t, err)
	configureZone(t, ctx, conn, `DATABASE kv`, zoneConfig{replicas: 3, onlyNodes: []int{4, 5, 6}})

	c.Run(ctx, c.Node(6), `./cockroach workload init kv --splits 1000 {pgurl:1}`)

	// Move ranges to the appropriate nodes. Precreating the database/range and
	// moving it to the correct nodes first is not sufficient, since workload will
	// spread the ranges across all nodes regardless.
	relocateRanges(t, ctx, conn, `database_name = 'kv'`, []int{1, 2, 3}, []int{4, 5, 6})
	relocateRanges(t, ctx, conn, `database_name != 'kv'`, []int{4, 5, 6}, []int{1, 2, 3})

	// Check that we have a few split leaders/leaseholders on n4-n6. We give
	// it a few seconds, since metrics are updated every 10 seconds.
	for i := 0; ; i++ {
		var count float64
		for _, node := range []int{4, 5, 6} {
			count += nodeMetric(ctx, t, c, node, "replicas.leaders_not_leaseholders")
		}
		t.Status(fmt.Sprintf("%.0f split leaders/leaseholders", count))
		if count >= 3 {
			break
		} else if i >= 10 {
			t.Fatalf("timed out waiting for 3 split leaders/leaseholders")
		}
		time.Sleep(time.Second)
	}

	// Start workload on n7 using n1-n3 as gateways.
	t.Status("running workload")
	m := c.NewMonitor(ctx, c.Range(1, 6))
	m.Go(func(ctx context.Context) error {
		c.Run(ctx, c.Node(7), `./cockroach workload run kv --read-percent 50 `+
			`--duration 20m --concurrency 256 --max-rate 2048 --timeout 1m --tolerate-errors `+
			`--histograms=`+t.PerfArtifactsDir()+`/stats.json `+
			`{pgurl:1-3}`)
		return nil
	})

	// Start a worker to fail and recover partial partitions between each pair of
	// n4-n6 for 3 cycles (9 failures total).
	failer.Ready(ctx, m)
	m.Go(func(ctx context.Context) error {
		var raftCfg base.RaftConfig
		raftCfg.SetDefaults()

		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for i := 0; i < 3; i++ {
			for _, node := range []int{4, 5, 6} {
				select {
				case <-ticker.C:
				case <-ctx.Done():
					return ctx.Err()
				}

				randTimer := time.After(randutil.RandDuration(rng, raftCfg.RangeLeaseRenewalDuration()))

				// Ranges may occasionally escape their constraints. Move them to where
				// they should be.
				relocateRanges(t, ctx, conn, `database_name = 'kv'`, []int{1, 2, 3}, []int{4, 5, 6})
				relocateRanges(t, ctx, conn, `database_name != 'kv'`, []int{4, 5, 6}, []int{1, 2, 3})

				// Randomly sleep up to the lease renewal interval, to vary the time
				// between the last lease renewal and the failure. We start the timer
				// before the range relocation above to run them concurrently.
				select {
				case <-randTimer:
				case <-ctx.Done():
				}

				t.Status(fmt.Sprintf("failing n%d (blackhole lease/leader)", node))
				nextNode := node + 1
				if nextNode > 6 {
					nextNode = 4
				}
				failer.FailPartial(ctx, node, []int{nextNode})

				select {
				case <-ticker.C:
				case <-ctx.Done():
					return ctx.Err()
				}

				t.Status(fmt.Sprintf("recovering n%d (blackhole lease/leader)", node))
				failer.Recover(ctx, node)
			}
		}
		return nil
	})
	m.Wait()
}

// runFailoverPartialLeaseLiveness tests a partial network partition between a
// leaseholder and node liveness. With epoch leases we would normally expect
// this to recover shortly, since the node can't heartbeat its liveness record
// and thus its leases will expire. However, it will maintain Raft leadership,
// and we prevent non-leaders from acquiring leases, which can prevent the lease
// from moving unless we explicitly handle this. See also:
// https://github.com/cockroachdb/cockroach/pull/87244.
//
// Cluster topology:
//
// n1-n3: system ranges and SQL gateways
// n4:    liveness leaseholder
// n5-7:  user ranges
//
// A partial blackhole network partition is triggered between n4 and each of
// n5-n7 sequentially, 3 times per node for a total of 9 times. A kv50 workload
// is running against SQL gateways on n1-n3, and we collect the pMax latency for
// graphing.
func runFailoverPartialLeaseLiveness(
	ctx context.Context, t test.Test, c cluster.Cluster, expLeases bool,
) {
	require.Equal(t, 8, c.Spec().NodeCount)

	rng, _ := randutil.NewTestRand()

	// Create cluster.
	opts := option.DefaultStartOpts()
	settings := install.MakeClusterSettings()

	failer := makeFailer(t, c, failureModeBlackhole, opts, settings).(partialFailer)
	failer.Setup(ctx)
	defer failer.Cleanup(ctx)

	c.Put(ctx, t.Cockroach(), "./cockroach")
	c.Start(ctx, t.L(), opts, settings, c.Range(1, 7))

	conn := c.Conn(ctx, t.L(), 1)
	defer conn.Close()

	_, err := conn.ExecContext(ctx, `SET CLUSTER SETTING kv.expiration_leases_only.enabled = $1`,
		expLeases)
	require.NoError(t, err)

	// Place all ranges on n1-n3, and an extra liveness leaseholder replica on n4.
	configureAllZones(t, ctx, conn, zoneConfig{replicas: 3, onlyNodes: []int{1, 2, 3}})
	configureZone(t, ctx, conn, `RANGE liveness`, zoneConfig{
		replicas: 4, onlyNodes: []int{1, 2, 3, 4}, leaseNode: 4})

	// Wait for upreplication.
	require.NoError(t, WaitFor3XReplication(ctx, t, conn))

	// Create the kv database on n5-n7.
	t.Status("creating workload database")
	_, err = conn.ExecContext(ctx, `CREATE DATABASE kv`)
	require.NoError(t, err)
	configureZone(t, ctx, conn, `DATABASE kv`, zoneConfig{replicas: 3, onlyNodes: []int{5, 6, 7}})

	c.Run(ctx, c.Node(6), `./cockroach workload init kv --splits 1000 {pgurl:1}`)

	// The replicate queue takes forever to move the ranges, so we do it
	// ourselves. Precreating the database/range and moving it to the correct
	// nodes first is not sufficient, since workload will spread the ranges across
	// all nodes regardless.
	relocateRanges(t, ctx, conn, `database_name = 'kv'`, []int{1, 2, 3, 4}, []int{5, 6, 7})
	relocateRanges(t, ctx, conn, `database_name != 'kv'`, []int{5, 6, 7}, []int{1, 2, 3, 4})
	relocateRanges(t, ctx, conn, `range_id != 2`, []int{4}, []int{1, 2, 3})

	// Start workload on n8 using n1-n3 as gateways (not partitioned).
	t.Status("running workload")
	m := c.NewMonitor(ctx, c.Range(1, 7))
	m.Go(func(ctx context.Context) error {
		c.Run(ctx, c.Node(8), `./cockroach workload run kv --read-percent 50 `+
			`--duration 20m --concurrency 256 --max-rate 2048 --timeout 1m --tolerate-errors `+
			`--histograms=`+t.PerfArtifactsDir()+`/stats.json `+
			`{pgurl:1-3}`)
		return nil
	})

	// Start a worker to fail and recover partial partitions between n4 (liveness)
	// and workload leaseholders n5-n7 for 1 minute each, 3 times per node for 9
	// times total.
	failer.Ready(ctx, m)
	m.Go(func(ctx context.Context) error {
		var raftCfg base.RaftConfig
		raftCfg.SetDefaults()

		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for i := 0; i < 3; i++ {
			for _, node := range []int{5, 6, 7} {
				select {
				case <-ticker.C:
				case <-ctx.Done():
					return ctx.Err()
				}

				randTimer := time.After(randutil.RandDuration(rng, raftCfg.RangeLeaseRenewalDuration()))

				// Ranges and leases may occasionally escape their constraints. Move
				// them to where they should be.
				relocateRanges(t, ctx, conn, `database_name = 'kv'`, []int{1, 2, 3, 4}, []int{5, 6, 7})
				relocateRanges(t, ctx, conn, `database_name != 'kv'`, []int{node}, []int{1, 2, 3})
				relocateRanges(t, ctx, conn, `range_id = 2`, []int{5, 6, 7}, []int{1, 2, 3, 4})
				relocateLeases(t, ctx, conn, `range_id = 2`, 4)

				// Randomly sleep up to the lease renewal interval, to vary the time
				// between the last lease renewal and the failure. We start the timer
				// before the range relocation above to run them concurrently.
				select {
				case <-randTimer:
				case <-ctx.Done():
				}

				t.Status(fmt.Sprintf("failing n%d (blackhole lease/liveness)", node))
				failer.FailPartial(ctx, node, []int{4})

				select {
				case <-ticker.C:
				case <-ctx.Done():
					return ctx.Err()
				}

				t.Status(fmt.Sprintf("recovering n%d (blackhole lease/liveness)", node))
				failer.Recover(ctx, node)
			}
		}
		return nil
	})
	m.Wait()
}

// runFailoverNonSystem benchmarks the maximum duration of range unavailability
// following a leaseholder failure with only non-system ranges.
//
//   - No system ranges located on the failed node.
//
//   - SQL clients do not connect to the failed node.
//
//   - The workload consists of individual point reads and writes.
//
// Since the lease unavailability is probabilistic, depending e.g. on the time
// since the last heartbeat and other variables, we run 9 failures and record
// the pMax latency to find the upper bound on unavailability. We expect this
// worst-case latency to be slightly larger than the lease interval (9s), to
// account for lease acquisition and retry latencies. We do not assert this, but
// instead export latency histograms for graphing.
//
// The cluster layout is as follows:
//
// n1-n3: System ranges and SQL gateways.
// n4-n6: Workload ranges.
// n7:    Workload runner.
//
// The test runs a kv50 workload with batch size 1, using 256 concurrent workers
// directed at n1-n3 with a rate of 2048 reqs/s. n4-n6 fail and recover in
// order, with 1 minute between each operation, for 3 cycles totaling 9
// failures.
func runFailoverNonSystem(
	ctx context.Context, t test.Test, c cluster.Cluster, failureMode failureMode, expLeases bool,
) {
	require.Equal(t, 7, c.Spec().NodeCount)

	rng, _ := randutil.NewTestRand()

	// Create cluster.
	opts := option.DefaultStartOpts()
	settings := install.MakeClusterSettings()

	failer := makeFailer(t, c, failureMode, opts, settings)
	failer.Setup(ctx)
	defer failer.Cleanup(ctx)

	c.Put(ctx, t.Cockroach(), "./cockroach")
	c.Start(ctx, t.L(), opts, settings, c.Range(1, 6))

	conn := c.Conn(ctx, t.L(), 1)
	defer conn.Close()

	// Configure cluster. This test controls the ranges manually.
	t.Status("configuring cluster")
	_, err := conn.ExecContext(ctx, `SET CLUSTER SETTING kv.range_split.by_load_enabled = 'false'`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `SET CLUSTER SETTING kv.expiration_leases_only.enabled = $1`,
		expLeases)
	require.NoError(t, err)

	// Constrain all existing zone configs to n1-n3.
	configureAllZones(t, ctx, conn, zoneConfig{replicas: 3, onlyNodes: []int{1, 2, 3}})

	// Wait for upreplication.
	require.NoError(t, WaitFor3XReplication(ctx, t, conn))

	// Create the kv database, constrained to n4-n6. Despite the zone config, the
	// ranges will initially be distributed across all cluster nodes.
	t.Status("creating workload database")
	_, err = conn.ExecContext(ctx, `CREATE DATABASE kv`)
	require.NoError(t, err)
	configureZone(t, ctx, conn, `DATABASE kv`, zoneConfig{replicas: 3, onlyNodes: []int{4, 5, 6}})
	c.Run(ctx, c.Node(7), `./cockroach workload init kv --splits 1000 {pgurl:1}`)

	// The replicate queue takes forever to move the kv ranges from n1-n3 to
	// n4-n6, so we do it ourselves. Precreating the database/range and moving it
	// to the correct nodes first is not sufficient, since workload will spread
	// the ranges across all nodes regardless.
	relocateRanges(t, ctx, conn, `database_name = 'kv'`, []int{1, 2, 3}, []int{4, 5, 6})

	// Start workload on n7, using n1-n3 as gateways. Run it for 20
	// minutes, since we take ~2 minutes to fail and recover each node, and
	// we do 3 cycles of each of the 3 nodes in order.
	t.Status("running workload")
	m := c.NewMonitor(ctx, c.Range(1, 6))
	m.Go(func(ctx context.Context) error {
		c.Run(ctx, c.Node(7), `./cockroach workload run kv --read-percent 50 `+
			`--duration 20m --concurrency 256 --max-rate 2048 --timeout 1m --tolerate-errors `+
			`--histograms=`+t.PerfArtifactsDir()+`/stats.json `+
			`{pgurl:1-3}`)
		return nil
	})

	// Start a worker to fail and recover n4-n6 in order.
	failer.Ready(ctx, m)
	m.Go(func(ctx context.Context) error {
		var raftCfg base.RaftConfig
		raftCfg.SetDefaults()

		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for i := 0; i < 3; i++ {
			for _, node := range []int{4, 5, 6} {
				select {
				case <-ticker.C:
				case <-ctx.Done():
					return ctx.Err()
				}

				randTimer := time.After(randutil.RandDuration(rng, raftCfg.RangeLeaseRenewalDuration()))

				// Ranges may occasionally escape their constraints. Move them
				// to where they should be.
				relocateRanges(t, ctx, conn, `database_name = 'kv'`, []int{1, 2, 3}, []int{4, 5, 6})
				relocateRanges(t, ctx, conn, `database_name != 'kv'`, []int{node}, []int{1, 2, 3})

				// Randomly sleep up to the lease renewal interval, to vary the time
				// between the last lease renewal and the failure. We start the timer
				// before the range relocation above to run them concurrently.
				select {
				case <-randTimer:
				case <-ctx.Done():
				}

				t.Status(fmt.Sprintf("failing n%d (%s)", node, failureMode))
				failer.Fail(ctx, node)

				select {
				case <-ticker.C:
				case <-ctx.Done():
					return ctx.Err()
				}

				t.Status(fmt.Sprintf("recovering n%d (%s)", node, failureMode))
				failer.Recover(ctx, node)
			}
		}
		return nil
	})
	m.Wait()
}

// runFailoverLiveness benchmarks the maximum duration of *user* range
// unavailability following a liveness-only leaseholder failure. When the
// liveness range becomes unavailable, other nodes are unable to heartbeat and
// extend their leases, and their leases may thus expire as well making them
// unavailable.
//
//   - Only liveness range located on the failed node, as leaseholder.
//
//   - SQL clients do not connect to the failed node.
//
//   - The workload consists of individual point reads and writes.
//
// Since the range unavailability is probabilistic, depending e.g. on the time
// since the last heartbeat and other variables, we run 9 failures and record
// the number of expired leases on n1-n3 as well as the pMax latency to find the
// upper bound on unavailability. We do not assert anything, but instead export
// metrics for graphing.
//
// The cluster layout is as follows:
//
// n1-n3: All ranges, including liveness.
// n4:    Liveness range leaseholder.
// n5:    Workload runner.
//
// The test runs a kv50 workload with batch size 1, using 256 concurrent workers
// directed at n1-n3 with a rate of 2048 reqs/s. n4 fails and recovers, with 1
// minute between each operation, for 9 cycles.
//
// TODO(erikgrinaker): The metrics resolution of 10 seconds isn't really good
// enough to accurately measure the number of invalid leases, but it's what we
// have currently. Prometheus scraping more often isn't enough, because CRDB
// itself only samples every 10 seconds.
func runFailoverLiveness(
	ctx context.Context, t test.Test, c cluster.Cluster, failureMode failureMode, expLeases bool,
) {
	require.Equal(t, 5, c.Spec().NodeCount)

	rng, _ := randutil.NewTestRand()

	// Create cluster. Don't schedule a backup as this roachtest reports to roachperf.
	opts := option.DefaultStartOptsNoBackups()
	settings := install.MakeClusterSettings()

	failer := makeFailer(t, c, failureMode, opts, settings)
	failer.Setup(ctx)
	defer failer.Cleanup(ctx)

	c.Put(ctx, t.Cockroach(), "./cockroach")
	c.Start(ctx, t.L(), opts, settings, c.Range(1, 4))

	conn := c.Conn(ctx, t.L(), 1)
	defer conn.Close()

	// Configure cluster. This test controls the ranges manually.
	t.Status("configuring cluster")
	_, err := conn.ExecContext(ctx, `SET CLUSTER SETTING kv.range_split.by_load_enabled = 'false'`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `SET CLUSTER SETTING kv.expiration_leases_only.enabled = $1`,
		expLeases)
	require.NoError(t, err)

	// Constrain all existing zone configs to n1-n3.
	configureAllZones(t, ctx, conn, zoneConfig{replicas: 3, onlyNodes: []int{1, 2, 3}})

	// Constrain the liveness range to n1-n4, with leaseholder preference on n4.
	configureZone(t, ctx, conn, `RANGE liveness`, zoneConfig{replicas: 4, leaseNode: 4})
	require.NoError(t, err)

	// Wait for upreplication.
	require.NoError(t, WaitFor3XReplication(ctx, t, conn))

	// Create the kv database, constrained to n1-n3. Despite the zone config, the
	// ranges will initially be distributed across all cluster nodes.
	t.Status("creating workload database")
	_, err = conn.ExecContext(ctx, `CREATE DATABASE kv`)
	require.NoError(t, err)
	configureZone(t, ctx, conn, `DATABASE kv`, zoneConfig{replicas: 3, onlyNodes: []int{1, 2, 3}})
	c.Run(ctx, c.Node(5), `./cockroach workload init kv --splits 1000 {pgurl:1}`)

	// The replicate queue takes forever to move the other ranges off of n4 so we
	// do it ourselves. Precreating the database/range and moving it to the
	// correct nodes first is not sufficient, since workload will spread the
	// ranges across all nodes regardless.
	relocateRanges(t, ctx, conn, `range_id != 2`, []int{4}, []int{1, 2, 3})

	// We also make sure the lease is located on n4.
	relocateLeases(t, ctx, conn, `range_id = 2`, 4)

	// Start workload on n7, using n1-n3 as gateways. Run it for 20 minutes, since
	// we take ~2 minutes to fail and recover the node, and we do 9 cycles.
	t.Status("running workload")
	m := c.NewMonitor(ctx, c.Range(1, 4))
	m.Go(func(ctx context.Context) error {
		c.Run(ctx, c.Node(5), `./cockroach workload run kv --read-percent 50 `+
			`--duration 20m --concurrency 256 --max-rate 2048 --timeout 1m --tolerate-errors `+
			`--histograms=`+t.PerfArtifactsDir()+`/stats.json `+
			`{pgurl:1-3}`)
		return nil
	})

	// Start a worker to fail and recover n4.
	failer.Ready(ctx, m)
	m.Go(func(ctx context.Context) error {
		var raftCfg base.RaftConfig
		raftCfg.SetDefaults()

		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for i := 0; i < 9; i++ {
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return ctx.Err()
			}

			randTimer := time.After(randutil.RandDuration(rng, raftCfg.RangeLeaseRenewalDuration()))

			// Ranges and leases may occasionally escape their constraints. Move them
			// to where they should be.
			relocateRanges(t, ctx, conn, `range_id != 2`, []int{4}, []int{1, 2, 3})
			relocateLeases(t, ctx, conn, `range_id = 2`, 4)

			// Randomly sleep up to the lease renewal interval, to vary the time
			// between the last lease renewal and the failure. We start the timer
			// before the range relocation above to run them concurrently.
			select {
			case <-randTimer:
			case <-ctx.Done():
			}

			t.Status(fmt.Sprintf("failing n%d (%s)", 4, failureMode))
			failer.Fail(ctx, 4)

			select {
			case <-ticker.C:
			case <-ctx.Done():
				return ctx.Err()
			}

			t.Status(fmt.Sprintf("recovering n%d (%s)", 4, failureMode))
			failer.Recover(ctx, 4)
			relocateLeases(t, ctx, conn, `range_id = 2`, 4)
		}
		return nil
	})
	m.Wait()
}

// runFailoverSystemNonLiveness benchmarks the maximum duration of range
// unavailability following a leaseholder failure with only system ranges,
// excluding the liveness range which is tested separately in
// runFailoverLiveness.
//
//   - No user or liveness ranges located on the failed node.
//
//   - SQL clients do not connect to the failed node.
//
//   - The workload consists of individual point reads and writes.
//
// Since the lease unavailability is probabilistic, depending e.g. on the time
// since the last heartbeat and other variables, we run 9 failures and record
// the pMax latency to find the upper bound on unavailability. Ideally, losing
// the lease on these ranges should have no impact on the user traffic.
//
// The cluster layout is as follows:
//
// n1-n3: Workload ranges, liveness range, and SQL gateways.
// n4-n6: System ranges excluding liveness.
// n7:    Workload runner.
//
// The test runs a kv50 workload with batch size 1, using 256 concurrent workers
// directed at n1-n3 with a rate of 2048 reqs/s. n4-n6 fail and recover in
// order, with 1 minute between each operation, for 3 cycles totaling 9
// failures.
func runFailoverSystemNonLiveness(
	ctx context.Context, t test.Test, c cluster.Cluster, failureMode failureMode, expLeases bool,
) {
	require.Equal(t, 7, c.Spec().NodeCount)

	rng, _ := randutil.NewTestRand()

	// Create cluster.
	opts := option.DefaultStartOpts()
	settings := install.MakeClusterSettings()

	failer := makeFailer(t, c, failureMode, opts, settings)
	failer.Setup(ctx)
	defer failer.Cleanup(ctx)

	c.Put(ctx, t.Cockroach(), "./cockroach")
	c.Start(ctx, t.L(), opts, settings, c.Range(1, 6))

	conn := c.Conn(ctx, t.L(), 1)
	defer conn.Close()

	// Configure cluster. This test controls the ranges manually.
	t.Status("configuring cluster")
	_, err := conn.ExecContext(ctx, `SET CLUSTER SETTING kv.range_split.by_load_enabled = 'false'`)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, `SET CLUSTER SETTING kv.expiration_leases_only.enabled = $1`,
		expLeases)
	require.NoError(t, err)

	// Constrain all existing zone configs to n4-n6, except liveness which is
	// constrained to n1-n3.
	configureAllZones(t, ctx, conn, zoneConfig{replicas: 3, onlyNodes: []int{4, 5, 6}})
	configureZone(t, ctx, conn, `RANGE liveness`, zoneConfig{replicas: 3, onlyNodes: []int{1, 2, 3}})
	require.NoError(t, err)

	// Wait for upreplication.
	require.NoError(t, WaitFor3XReplication(ctx, t, conn))

	// Create the kv database, constrained to n1-n3. Despite the zone config, the
	// ranges will initially be distributed across all cluster nodes.
	t.Status("creating workload database")
	_, err = conn.ExecContext(ctx, `CREATE DATABASE kv`)
	require.NoError(t, err)
	configureZone(t, ctx, conn, `DATABASE kv`, zoneConfig{replicas: 3, onlyNodes: []int{1, 2, 3}})
	c.Run(ctx, c.Node(7), `./cockroach workload init kv --splits 1000 {pgurl:1}`)

	// The replicate queue takes forever to move the kv ranges from n4-n6 to
	// n1-n3, so we do it ourselves. Precreating the database/range and moving it
	// to the correct nodes first is not sufficient, since workload will spread
	// the ranges across all nodes regardless.
	relocateRanges(t, ctx, conn, `database_name = 'kv' OR range_id = 2`,
		[]int{4, 5, 6}, []int{1, 2, 3})
	relocateRanges(t, ctx, conn, `database_name != 'kv' AND range_id != 2`,
		[]int{1, 2, 3}, []int{4, 5, 6})

	// Start workload on n7, using n1-n3 as gateways. Run it for 20 minutes, since
	// we take ~2 minutes to fail and recover each node, and we do 3 cycles of each
	// of the 3 nodes in order.
	t.Status("running workload")
	m := c.NewMonitor(ctx, c.Range(1, 6))
	m.Go(func(ctx context.Context) error {
		c.Run(ctx, c.Node(7), `./cockroach workload run kv --read-percent 50 `+
			`--duration 20m --concurrency 256 --max-rate 2048 --timeout 1m --tolerate-errors `+
			`--histograms=`+t.PerfArtifactsDir()+`/stats.json `+
			`{pgurl:1-3}`)
		return nil
	})

	// Start a worker to fail and recover n4-n6 in order.
	failer.Ready(ctx, m)
	m.Go(func(ctx context.Context) error {
		var raftCfg base.RaftConfig
		raftCfg.SetDefaults()

		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		for i := 0; i < 3; i++ {
			for _, node := range []int{4, 5, 6} {
				select {
				case <-ticker.C:
				case <-ctx.Done():
					return ctx.Err()
				}

				randTimer := time.After(randutil.RandDuration(rng, raftCfg.RangeLeaseRenewalDuration()))

				// Ranges may occasionally escape their constraints. Move them
				// to where they should be.
				relocateRanges(t, ctx, conn, `database_name != 'kv' AND range_id != 2`,
					[]int{1, 2, 3}, []int{4, 5, 6})
				relocateRanges(t, ctx, conn, `database_name = 'kv' OR range_id = 2`,
					[]int{4, 5, 6}, []int{1, 2, 3})

				// Randomly sleep up to the lease renewal interval, to vary the time
				// between the last lease renewal and the failure. We start the timer
				// before the range relocation above to run them concurrently.
				select {
				case <-randTimer:
				case <-ctx.Done():
				}

				t.Status(fmt.Sprintf("failing n%d (%s)", node, failureMode))
				failer.Fail(ctx, node)

				select {
				case <-ticker.C:
				case <-ctx.Done():
					return ctx.Err()
				}

				t.Status(fmt.Sprintf("recovering n%d (%s)", node, failureMode))
				failer.Recover(ctx, node)
			}
		}
		return nil
	})
	m.Wait()
}

// failureMode specifies a failure mode.
type failureMode string

const (
	failureModeBlackhole     failureMode = "blackhole"
	failureModeBlackholeRecv failureMode = "blackhole-recv"
	failureModeBlackholeSend failureMode = "blackhole-send"
	failureModeCrash         failureMode = "crash"
	failureModeDiskStall     failureMode = "disk-stall"
	failureModePause         failureMode = "pause"
)

// makeFailer creates a new failer for the given failureMode.
func makeFailer(
	t test.Test,
	c cluster.Cluster,
	failureMode failureMode,
	opts option.StartOpts,
	settings install.ClusterSettings,
) failer {
	switch failureMode {
	case failureModeBlackhole:
		return &blackholeFailer{
			t:      t,
			c:      c,
			input:  true,
			output: true,
		}
	case failureModeBlackholeRecv:
		return &blackholeFailer{
			t:     t,
			c:     c,
			input: true,
		}
	case failureModeBlackholeSend:
		return &blackholeFailer{
			t:      t,
			c:      c,
			output: true,
		}
	case failureModeCrash:
		return &crashFailer{
			t:             t,
			c:             c,
			startOpts:     opts,
			startSettings: settings,
		}
	case failureModeDiskStall:
		// TODO(baptist): This mode doesn't work on local clusters since
		// dmsetupDiskStaller does not support local clusters. Either support could
		// be added for it or there could be a flag to not fatal when run in local
		// mode. The net impact is that this failure can't be simulated on local
		// clusters today.
		return &diskStallFailer{
			t:             t,
			c:             c,
			startOpts:     opts,
			startSettings: settings,
			staller:       &dmsetupDiskStaller{t: t, c: c},
		}
	case failureModePause:
		return &pauseFailer{
			t: t,
			c: c,
		}
	default:
		t.Fatalf("unknown failure mode %s", failureMode)
		return nil
	}
}

// failer fails and recovers a given node in some particular way.
type failer interface {
	// Setup prepares the failer. It is called before the cluster is started.
	Setup(ctx context.Context)

	// Ready is called when the cluster is ready, with a running workload.
	Ready(ctx context.Context, m cluster.Monitor)

	// Cleanup cleans up when the test exits. This is needed e.g. when the cluster
	// is reused by a different test.
	Cleanup(ctx context.Context)

	// Fail fails the given node.
	Fail(ctx context.Context, nodeID int)

	// Recover recovers the given node.
	Recover(ctx context.Context, nodeID int)
}

// partialFailer supports partial failures between specific node pairs.
type partialFailer interface {
	failer

	// FailPartial fails the node for the given peers.
	FailPartial(ctx context.Context, nodeID int, peerIDs []int)
}

// blackholeFailer causes a network failure where TCP/IP packets to/from port
// 26257 are dropped, causing network hangs and timeouts.
//
// If only one if input or output are enabled, connections in that direction
// will fail (even already established connections), but connections in the
// other direction are still functional (including responses).
type blackholeFailer struct {
	t      test.Test
	c      cluster.Cluster
	input  bool
	output bool
}

func (f *blackholeFailer) Setup(_ context.Context)                    {}
func (f *blackholeFailer) Ready(_ context.Context, _ cluster.Monitor) {}

func (f *blackholeFailer) Cleanup(ctx context.Context) {
	if f.c.IsLocal() {
		f.t.Status("skipping blackhole cleanup on local cluster")
		return
	}
	f.c.Run(ctx, f.c.All(), `sudo iptables -F`)
}

func (f *blackholeFailer) Fail(ctx context.Context, nodeID int) {
	if f.c.IsLocal() {
		f.t.Status("skipping blackhole failure on local cluster")
		return
	}
	// When dropping both input and output, make sure we drop packets in both
	// directions for both the inbound and outbound TCP connections, such that we
	// get a proper black hole. Only dropping one direction for both of INPUT and
	// OUTPUT will still let e.g. TCP retransmits through, which may affect the
	// TCP stack behavior and is not representative of real network outages.
	//
	// For the asymmetric partitions, only drop packets in one direction since
	// this is representative of accidental firewall rules we've seen cause such
	// outages in the wild.
	if f.input && f.output {
		// Inbound TCP connections, both received and sent packets.
		f.c.Run(ctx, f.c.Node(nodeID), `sudo iptables -A INPUT -p tcp --dport 26257 -j DROP`)
		f.c.Run(ctx, f.c.Node(nodeID), `sudo iptables -A OUTPUT -p tcp --sport 26257 -j DROP`)
		// Outbound TCP connections, both sent and received packets.
		f.c.Run(ctx, f.c.Node(nodeID), `sudo iptables -A OUTPUT -p tcp --dport 26257 -j DROP`)
		f.c.Run(ctx, f.c.Node(nodeID), `sudo iptables -A INPUT -p tcp --sport 26257 -j DROP`)
	} else if f.input {
		f.c.Run(ctx, f.c.Node(nodeID), `sudo iptables -A INPUT -p tcp --dport 26257 -j DROP`)
	} else if f.output {
		f.c.Run(ctx, f.c.Node(nodeID), `sudo iptables -A OUTPUT -p tcp --dport 26257 -j DROP`)
	}
}

// FailPartial creates a partial blackhole failure between the given node and
// peers.
func (f *blackholeFailer) FailPartial(ctx context.Context, nodeID int, peerIDs []int) {
	if f.c.IsLocal() {
		f.t.Status("skipping blackhole failure on local cluster")
		return
	}
	peerIPs, err := f.c.InternalIP(ctx, f.t.L(), peerIDs)
	require.NoError(f.t, err)

	for _, peerIP := range peerIPs {
		// When dropping both input and output, make sure we drop packets in both
		// directions for both the inbound and outbound TCP connections, such that
		// we get a proper black hole. Only dropping one direction for both of INPUT
		// and OUTPUT will still let e.g. TCP retransmits through, which may affect
		// TCP stack behavior and is not representative of real network outages.
		//
		// For the asymmetric partitions, only drop packets in one direction since
		// this is representative of accidental firewall rules we've seen cause such
		// outages in the wild.
		if f.input && f.output {
			// Inbound TCP connections, both received and sent packets.
			f.c.Run(ctx, f.c.Node(nodeID), fmt.Sprintf(
				`sudo iptables -A INPUT -p tcp -s %s --dport 26257 -j DROP`, peerIP))
			f.c.Run(ctx, f.c.Node(nodeID), fmt.Sprintf(
				`sudo iptables -A OUTPUT -p tcp -d %s --sport 26257 -j DROP`, peerIP))
			// Outbound TCP connections, both sent and received packets.
			f.c.Run(ctx, f.c.Node(nodeID), fmt.Sprintf(
				`sudo iptables -A OUTPUT -p tcp -d %s --dport 26257 -j DROP`, peerIP))
			f.c.Run(ctx, f.c.Node(nodeID), fmt.Sprintf(
				`sudo iptables -A INPUT -p tcp -s %s --sport 26257 -j DROP`, peerIP))
		} else if f.input {
			f.c.Run(ctx, f.c.Node(nodeID), fmt.Sprintf(
				`sudo iptables -A INPUT -p tcp -s %s --dport 26257 -j DROP`, peerIP))
		} else if f.output {
			f.c.Run(ctx, f.c.Node(nodeID), fmt.Sprintf(
				`sudo iptables -A OUTPUT -p tcp -d %s --dport 26257 -j DROP`, peerIP))
		}
	}
}

func (f *blackholeFailer) Recover(ctx context.Context, nodeID int) {
	if f.c.IsLocal() {
		f.t.Status("skipping blackhole recovery on local cluster")
		return
	}
	f.c.Run(ctx, f.c.Node(nodeID), `sudo iptables -F`)
}

// crashFailer is a process crash where the TCP/IP stack remains responsive
// and sends immediate RST packets to peers.
type crashFailer struct {
	t             test.Test
	c             cluster.Cluster
	m             cluster.Monitor
	startOpts     option.StartOpts
	startSettings install.ClusterSettings
}

func (f *crashFailer) Setup(_ context.Context)                    {}
func (f *crashFailer) Ready(_ context.Context, m cluster.Monitor) { f.m = m }
func (f *crashFailer) Cleanup(_ context.Context)                  {}

func (f *crashFailer) Fail(ctx context.Context, nodeID int) {
	f.m.ExpectDeath()
	f.c.Stop(ctx, f.t.L(), option.DefaultStopOpts(), f.c.Node(nodeID)) // uses SIGKILL
}

func (f *crashFailer) Recover(ctx context.Context, nodeID int) {
	f.c.Start(ctx, f.t.L(), f.startOpts, f.startSettings, f.c.Node(nodeID))
}

// diskStallFailer stalls the disk indefinitely. This should cause the node to
// eventually self-terminate, but we'd want leases to move off before then.
type diskStallFailer struct {
	t             test.Test
	c             cluster.Cluster
	m             cluster.Monitor
	startOpts     option.StartOpts
	startSettings install.ClusterSettings
	staller       diskStaller
}

func (f *diskStallFailer) Setup(ctx context.Context) {
	f.staller.Setup(ctx)
}

func (f *diskStallFailer) Ready(_ context.Context, m cluster.Monitor) {
	f.m = m
}

func (f *diskStallFailer) Cleanup(ctx context.Context) {
	f.staller.Unstall(ctx, f.c.All())
	// We have to stop the cluster before cleaning up the staller.
	f.m.ExpectDeaths(int32(f.c.Spec().NodeCount))
	f.c.Stop(ctx, f.t.L(), option.DefaultStopOpts(), f.c.All())
	f.staller.Cleanup(ctx)
}

func (f *diskStallFailer) Fail(ctx context.Context, nodeID int) {
	// Pebble's disk stall detector should crash the node.
	f.m.ExpectDeath()
	f.staller.Stall(ctx, f.c.Node(nodeID))
}

func (f *diskStallFailer) Recover(ctx context.Context, nodeID int) {
	f.staller.Unstall(ctx, f.c.Node(nodeID))
	// Pebble's disk stall detector should have terminated the node, but in case
	// it didn't, we explicitly stop it first.
	f.c.Stop(ctx, f.t.L(), option.DefaultStopOpts(), f.c.Node(nodeID))
	f.c.Start(ctx, f.t.L(), f.startOpts, f.startSettings, f.c.Node(nodeID))
}

// pauseFailer pauses the process, but keeps the OS (and thus network
// connections) alive.
type pauseFailer struct {
	t test.Test
	c cluster.Cluster
}

func (f *pauseFailer) Setup(ctx context.Context)   {}
func (f *pauseFailer) Cleanup(ctx context.Context) {}

func (f *pauseFailer) Ready(ctx context.Context, m cluster.Monitor) {
	// The process pause can trip the disk stall detector, so we disable it.
	conn := f.c.Conn(ctx, f.t.L(), 1)
	_, err := conn.ExecContext(ctx, `SET CLUSTER SETTING storage.max_sync_duration.fatal.enabled = false`)
	require.NoError(f.t, err)
}

func (f *pauseFailer) Fail(ctx context.Context, nodeID int) {
	f.c.Signal(ctx, f.t.L(), 19, f.c.Node(nodeID)) // SIGSTOP
}

func (f *pauseFailer) Recover(ctx context.Context, nodeID int) {
	f.c.Signal(ctx, f.t.L(), 18, f.c.Node(nodeID)) // SIGCONT
}

// waitForUpreplication waits for upreplication of ranges that satisfy the
// given predicate (using SHOW RANGES).
//
// TODO(erikgrinaker): move this into WaitForReplication() when it can use SHOW
// RANGES, i.e. when it's no longer needed in mixed-version tests with older
// versions that don't have SHOW RANGES.
func waitForUpreplication(
	t test.Test, ctx context.Context, conn *gosql.DB, predicate string, replicationFactor int,
) {
	var count int
	where := fmt.Sprintf("WHERE array_length(replicas, 1) < %d", replicationFactor)
	if predicate != "" {
		where += fmt.Sprintf(" AND (%s)", predicate)
	}
	for {
		require.NoError(t, conn.QueryRowContext(ctx,
			`SELECT count(distinct range_id) FROM [SHOW CLUSTER RANGES WITH TABLES, DETAILS] `+where).
			Scan(&count))
		if count == 0 {
			break
		}
		t.Status(fmt.Sprintf("waiting for %d ranges to upreplicate (%s)", count, predicate))
		time.Sleep(time.Second)
	}
}

// relocateRanges relocates all ranges matching the given predicate from a set
// of nodes to a different set of nodes. Moves are attempted sequentially from
// each source onto each target, and errors are retried indefinitely.
func relocateRanges(
	t test.Test, ctx context.Context, conn *gosql.DB, predicate string, from, to []int,
) {
	require.NotEmpty(t, predicate)
	var count int
	for _, source := range from {
		where := fmt.Sprintf("(%s) AND %d = ANY(replicas)", predicate, source)
		for {
			require.NoError(t, conn.QueryRowContext(ctx,
				`SELECT count(distinct range_id) FROM [SHOW CLUSTER RANGES WITH TABLES] WHERE `+where).
				Scan(&count))
			if count == 0 {
				break
			}
			t.Status(fmt.Sprintf("moving %d ranges off of n%d (%s)", count, source, predicate))
			for _, target := range to {
				_, err := conn.ExecContext(ctx, `ALTER RANGE RELOCATE FROM $1::int TO $2::int FOR `+
					`SELECT DISTINCT range_id FROM [SHOW CLUSTER RANGES WITH TABLES] WHERE `+where,
					source, target)
				if err != nil {
					t.Status(fmt.Sprintf("failed to move ranges: %s", err))
				}
			}
			time.Sleep(time.Second)
		}
	}
}

// relocateLeases relocates all leases matching the given predicate to the
// given node. Errors and failures are retried indefinitely.
func relocateLeases(t test.Test, ctx context.Context, conn *gosql.DB, predicate string, to int) {
	require.NotEmpty(t, predicate)
	var count int
	where := fmt.Sprintf("%s AND lease_holder != %d", predicate, to)
	for {
		require.NoError(t, conn.QueryRowContext(ctx,
			`SELECT count(distinct range_id) FROM [SHOW CLUSTER RANGES WITH TABLES, DETAILS] WHERE `+
				where).
			Scan(&count))
		if count == 0 {
			break
		}
		t.Status(fmt.Sprintf("moving %d leases to n%d (%s)", count, to, predicate))
		_, err := conn.ExecContext(ctx, `ALTER RANGE RELOCATE LEASE TO $1::int FOR `+
			`SELECT DISTINCT range_id FROM [SHOW CLUSTER RANGES WITH TABLES, DETAILS] WHERE `+where, to)
		if err != nil {
			t.Status(fmt.Sprintf("failed to move leases: %s", err))
		}
		time.Sleep(time.Second)
	}
}

type zoneConfig struct {
	replicas  int
	onlyNodes []int
	leaseNode int
}

// configureZone sets the zone config for the given target.
func configureZone(
	t test.Test, ctx context.Context, conn *gosql.DB, target string, cfg zoneConfig,
) {
	require.NotZero(t, cfg.replicas, "num_replicas must be > 0")

	// If onlyNodes is given, invert the constraint and specify which nodes are
	// prohibited. Otherwise, the allocator may leave replicas outside of the
	// specified nodes.
	var constraintsString string
	if len(cfg.onlyNodes) > 0 {
		nodeCount := t.Spec().(*registry.TestSpec).Cluster.NodeCount - 1 // subtract worker node
		included := map[int]bool{}
		for _, nodeID := range cfg.onlyNodes {
			included[nodeID] = true
		}
		excluded := []int{}
		for nodeID := 1; nodeID <= nodeCount; nodeID++ {
			if !included[nodeID] {
				excluded = append(excluded, nodeID)
			}
		}
		for _, nodeID := range excluded {
			if len(constraintsString) > 0 {
				constraintsString += ","
			}
			constraintsString += fmt.Sprintf("-node%d", nodeID)
		}
	}

	var leaseString string
	if cfg.leaseNode > 0 {
		leaseString += fmt.Sprintf("[+node%d]", cfg.leaseNode)
	}

	query := fmt.Sprintf(
		`ALTER %s CONFIGURE ZONE USING num_replicas = %d, constraints = '[%s]', lease_preferences = '[%s]'`,
		target, cfg.replicas, constraintsString, leaseString)
	t.Status(query)
	_, err := conn.ExecContext(ctx, query)
	require.NoError(t, err)
}

// configureAllZones will set zone configuration for all targets in the
// clusters.
func configureAllZones(t test.Test, ctx context.Context, conn *gosql.DB, cfg zoneConfig) {
	rows, err := conn.QueryContext(ctx, `SELECT target FROM [SHOW ALL ZONE CONFIGURATIONS]`)
	require.NoError(t, err)
	for rows.Next() {
		var target string
		require.NoError(t, rows.Scan(&target))
		configureZone(t, ctx, conn, target, cfg)
	}
	require.NoError(t, rows.Err())
}

// nodeMetric fetches the given metric value from the given node.
func nodeMetric(
	ctx context.Context, t test.Test, c cluster.Cluster, node int, metric string,
) float64 {
	var value float64
	err := c.Conn(ctx, t.L(), node).QueryRowContext(
		ctx, `SELECT value FROM crdb_internal.node_metrics WHERE name = $1`, metric).Scan(&value)
	require.NoError(t, err)
	return value
}
