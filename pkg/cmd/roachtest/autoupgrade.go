// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package main

import (
	"context"
	"fmt"
	"runtime"
	"time"

	"github.com/cockroachdb/cockroach/pkg/testutils"
	"github.com/cockroachdb/cockroach/pkg/util/binfetcher"
	"github.com/pkg/errors"
)

// This test verifies that preserve_downgrade_option is respected and that in the
// absence of it the cluster auto-upgrades when this is safe.
//
// NB: if you're interested in mixed-version testing, don't look at this test
// but check out acceptance/version-upgrade.
func registerAutoUpgrade(r *testRegistry) {
	runAutoUpgrade := func(ctx context.Context, t *test, c *cluster, oldVersion string) {
		nodes := c.spec.NodeCount
		goos := ifLocal(runtime.GOOS, "linux")

		b, err := binfetcher.Download(ctx, binfetcher.Options{
			Binary:  "cockroach",
			Version: "v" + oldVersion,
			GOOS:    goos,
			GOARCH:  "amd64",
		})
		if err != nil {
			t.Fatal(err)
		}

		c.Put(ctx, b, "./cockroach", c.Range(1, nodes))

		// NB: remove startArgsDontEncrypt across this file once we're not running
		// roachtest against v2.1 any more (which would start a v2.0 cluster here).
		c.Start(ctx, t, c.Range(1, nodes), startArgsDontEncrypt)

		const stageDuration = 30 * time.Second
		const timeUntilStoreDead = 90 * time.Second
		const buff = 10 * time.Second

		sleep := func(ts time.Duration) error {
			t.WorkerStatus("sleeping")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(ts):
				return nil
			}
		}

		db := c.Conn(ctx, 1)
		defer db.Close()
		// Without this line, the test reliably fails (at least on OSX), presumably
		// because a connection to a node that gets restarted somehow sticks around
		// in the pool and throws an error at the next client using it (instead of
		// transparently reconnecting).
		db.SetMaxIdleConns(0)

		if _, err := db.ExecContext(ctx,
			"SET CLUSTER SETTING server.time_until_store_dead = $1", timeUntilStoreDead.String(),
		); err != nil {
			t.Fatal(err)
		}

		if err := sleep(stageDuration); err != nil {
			t.Fatal(err)
		}

		decommissionAndStop := func(node int) error {
			t.WorkerStatus("decomission")
			port := fmt.Sprintf("{pgport:%d}", node)
			// Note that the following command line needs to run against both v2.1
			// and the current branch. Do not change it in a manner that is
			// incompatible with 2.1.
			if err := c.RunE(ctx, c.Node(node), "./cockroach quit --decommission --insecure --port="+port); err != nil {
				return err
			}
			t.WorkerStatus("stop")
			c.Stop(ctx, c.Node(node))
			return nil
		}

		clusterVersion := func() (string, error) {
			var version string
			if err := db.QueryRowContext(ctx, `SHOW CLUSTER SETTING version`).Scan(&version); err != nil {
				return "", errors.Wrap(err, "determining cluster version")
			}
			return version, nil
		}

		oldVersion, err = clusterVersion()
		if err != nil {
			t.Fatal(err)
		}

		checkUpgraded := func() (bool, error) {
			upgradedVersion, err := clusterVersion()
			if err != nil {
				return false, err
			}
			return upgradedVersion != oldVersion, nil
		}

		checkDowngradeOption := func(version string) error {
			if _, err := db.ExecContext(ctx,
				"SET CLUSTER SETTING cluster.preserve_downgrade_option = $1;", version,
			); err == nil {
				return fmt.Errorf("cluster.preserve_downgrade_option shouldn't be set to any other values besides current cluster version; was able to set it to %s", version)
			} else if !testutils.IsError(err, "cannot set cluster.preserve_downgrade_option") {
				return err
			}
			return nil
		}

		// Now perform a rolling restart into the new binary, except the last node.
		for i := 1; i < nodes; i++ {
			t.WorkerStatus("upgrading ", i)
			if err := c.StopCockroachGracefullyOnNode(ctx, i); err != nil {
				t.Fatal(err)
			}
			c.Put(ctx, cockroach, "./cockroach", c.Node(i))
			c.Start(ctx, t, c.Node(i), startArgsDontEncrypt)
			if err := sleep(stageDuration); err != nil {
				t.Fatal(err)
			}

			// Check cluster version is not upgraded until all nodes are running the new version.
			if upgraded, err := checkUpgraded(); err != nil {
				t.Fatal(err)
			} else if upgraded {
				t.Fatal("cluster setting version shouldn't be upgraded before all nodes are running the new version")
			}
		}

		// Now stop a previously started node and upgrade the last node.
		// Check cluster version is not upgraded.
		if err := c.StopCockroachGracefullyOnNode(ctx, nodes-1); err != nil {
			t.Fatal(err)
		}
		if err := c.StopCockroachGracefullyOnNode(ctx, nodes); err != nil {
			t.Fatal(err)
		}
		c.Put(ctx, cockroach, "./cockroach", c.Node(nodes))
		c.Start(ctx, t, c.Node(nodes), startArgsDontEncrypt)
		if err := sleep(stageDuration); err != nil {
			t.Fatal(err)
		}

		if upgraded, err := checkUpgraded(); err != nil {
			t.Fatal(err)
		} else if upgraded {
			t.Fatal("cluster setting version shouldn't be upgraded before all non-decommissioned nodes are alive")
		}

		// Now decommission and stop n3.
		// The decommissioned nodes should not prevent auto upgrade.
		if err := decommissionAndStop(nodes - 2); err != nil {
			t.Fatal(err)
		}
		if err := sleep(timeUntilStoreDead + buff); err != nil {
			t.Fatal(err)
		}

		// Check cannot set cluster setting cluster.preserve_downgrade_option to any
		// value besides the old cluster version.
		if err := checkDowngradeOption("1.9"); err != nil {
			t.Fatal(err)
		}
		if err := checkDowngradeOption("99.9"); err != nil {
			t.Fatal(err)
		}

		// Set cluster setting cluster.preserve_downgrade_option to be current
		// cluster version to prevent upgrade.
		if _, err := db.ExecContext(ctx,
			"SET CLUSTER SETTING cluster.preserve_downgrade_option = $1;", oldVersion,
		); err != nil {
			t.Fatal(err)
		}
		if err := sleep(stageDuration); err != nil {
			t.Fatal(err)
		}

		// Restart the previously stopped node.
		c.Start(ctx, t, c.Node(nodes-1), startArgsDontEncrypt)
		if err := sleep(stageDuration); err != nil {
			t.Fatal(err)
		}

		t.WorkerStatus("check cluster version has not been upgraded")
		if upgraded, err := checkUpgraded(); err != nil {
			t.Fatal(err)
		} else if upgraded {
			t.Fatal("cluster setting version shouldn't be upgraded because cluster.preserve_downgrade_option is set properly")
		}

		// Check cannot set cluster setting version until cluster.preserve_downgrade_option
		// is cleared.
		if _, err := db.ExecContext(ctx,
			"SET CLUSTER SETTING version = crdb_internal.node_executable_version();",
		); err == nil {
			t.Fatal("should not be able to set cluster setting version before resetting cluster.preserve_downgrade_option")
		} else if !testutils.IsError(err, "cluster.preserve_downgrade_option is set to") {
			t.Fatal(err)
		}

		// Reset cluster.preserve_downgrade_option to enable upgrade.
		if _, err := db.ExecContext(ctx,
			"RESET CLUSTER SETTING cluster.preserve_downgrade_option;",
		); err != nil {
			t.Fatal(err)
		}
		if err := sleep(stageDuration); err != nil {
			t.Fatal(err)
		}

		// Check if the cluster version has been upgraded.
		t.WorkerStatus("check cluster version has been upgraded")
		if upgraded, err := checkUpgraded(); err != nil {
			t.Fatal(err)
		} else if !upgraded {
			t.Fatalf("cluster setting version is not upgraded, still %s", oldVersion)
		}

		// Finally, check if the cluster.preserve_downgrade_option has been reset.
		t.WorkerStatus("check cluster setting cluster.preserve_downgrade_option has been set to an empty string")
		var downgradeVersion string
		if err := db.QueryRowContext(ctx,
			"SHOW CLUSTER SETTING cluster.preserve_downgrade_option",
		).Scan(&downgradeVersion); err != nil {
			t.Fatal(err)
		}
		if downgradeVersion != "" {
			t.Fatalf("cluster setting cluster.preserve_downgrade_option is %s, should be an empty string", downgradeVersion)
		}

		// Start n3 again to satisfy the dead node detector.
		c.Start(ctx, t, c.Node(nodes-2))
	}

	r.Add(testSpec{
		Name:       fmt.Sprintf("upgrade"),
		Owner:      OwnerKV,
		MinVersion: "v2.1.0",
		Cluster:    makeClusterSpec(5),
		Run: func(ctx context.Context, t *test, c *cluster) {
			pred, err := PredecessorVersion(r.buildVersion)
			if err != nil {
				t.Fatal(err)
			}
			runAutoUpgrade(ctx, t, c, pred)
		},
	})
}
