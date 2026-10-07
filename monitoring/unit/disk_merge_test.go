package monitoring

import "testing"

func deviceTotals(samples ...countedDisk) (uint64, uint64) {
	dev := newDiskDevice()
	for _, sample := range samples {
		dev.add(sample)
	}
	return dev.totals()
}

func TestMergePlainDiskKeepsLargerTotal(t *testing.T) {
	total, used := deviceTotals(
		countedDisk{total: 80, used: 70},
		countedDisk{total: 100, used: 40},
	)
	if total != 100 || used != 40 {
		t.Fatalf("got %d/%d, want 100/40", total, used)
	}
	total, used = deviceTotals(
		countedDisk{total: 100, used: 40},
		countedDisk{total: 80, used: 70},
	)
	if total != 100 || used != 40 {
		t.Fatalf("got %d/%d, want to keep the larger total", total, used)
	}
}

func TestMergeEqualTotalKeepsEarlierUsed(t *testing.T) {
	total, used := deviceTotals(
		countedDisk{total: 100, used: 1, subvol: "1"},
		countedDisk{total: 100, used: 99, subvol: "2"},
	)
	if total != 100 || used != 1 {
		t.Fatalf("equal totals must keep the earlier sample, got %d/%d", total, used)
	}
	total, used = deviceTotals(
		countedDisk{total: 100, used: 99, subvol: "2"},
		countedDisk{total: 100, used: 1, subvol: "1"},
	)
	if total != 100 || used != 99 {
		t.Fatalf("equal totals must keep the earlier sample, got %d/%d", total, used)
	}
}

func TestMergeSameSubvolumePrefersQuotaOverPool(t *testing.T) {
	quota := countedDisk{total: 1, used: 0, subvol: "256", quota: true}
	pool := countedDisk{total: 160, used: 9, subvol: "256", quota: false}
	total, used := deviceTotals(pool, quota)
	if total != 1 || used != 0 {
		t.Fatalf("quota must replace the same subvolume's pool view, got %d/%d", total, used)
	}
	total, used = deviceTotals(quota, pool)
	if total != 1 || used != 0 {
		t.Fatalf("pool view must not cover the same subvolume's quota, got %d/%d", total, used)
	}
}

func TestMergeDifferentSubvolumeKeepsPool(t *testing.T) {
	quota := countedDisk{total: 1, used: 0, subvol: "256", quota: true}
	pool := countedDisk{total: 160, used: 9, subvol: "5", quota: false}
	total, used := deviceTotals(pool, quota)
	if total != 160 || used != 9 {
		t.Fatalf("an unlimited subvolume must keep the pool, got %d/%d", total, used)
	}
	total, used = deviceTotals(quota, pool)
	if total != 160 || used != 9 {
		t.Fatalf("an unlimited subvolume must replace a smaller quota, got %d/%d", total, used)
	}
}

func TestMergeDifferentQuotasKeepTheLargerOne(t *testing.T) {
	small := countedDisk{total: 10, used: 1, subvol: "10", quota: true}
	large := countedDisk{total: 50, used: 2, subvol: "11", quota: true}
	total, used := deviceTotals(small, large)
	if total != 50 || used != 2 {
		t.Fatalf("different subvolume quotas must not be summed, got %d/%d", total, used)
	}
	total, used = deviceTotals(large, small)
	if total != 50 || used != 2 {
		t.Fatalf("the larger quota must stay, got %d/%d", total, used)
	}
}

func TestMergeSameSubvolumeKeepsTighterQuota(t *testing.T) {
	loose := countedDisk{total: 100, used: 10, subvol: "5", quota: true}
	tight := countedDisk{total: 40, used: 8, subvol: "5", quota: true}
	total, used := deviceTotals(loose, tight)
	if total != 40 || used != 8 {
		t.Fatalf("the tighter quota must win, got %d/%d", total, used)
	}
	total, used = deviceTotals(tight, loose)
	if total != 40 || used != 8 {
		t.Fatalf("a looser second reading must not raise the quota, got %d/%d", total, used)
	}
}

func TestUnlimitedSubvolumeSurvivesEveryQuotaOrder(t *testing.T) {
	poolA := countedDisk{total: 160, used: 9, subvol: "256", quota: false}
	poolB := countedDisk{total: 160, used: 9, subvol: "5", quota: false}
	quotaA := countedDisk{total: 1, used: 0, subvol: "256", quota: true}
	orders := [][]countedDisk{
		{poolA, poolB, quotaA},
		{poolA, quotaA, poolB},
		{poolB, poolA, quotaA},
		{poolB, quotaA, poolA},
		{quotaA, poolA, poolB},
		{quotaA, poolB, poolA},
	}
	for _, order := range orders {
		total, used := deviceTotals(order...)
		if total != 160 || used != 9 {
			t.Fatalf("order %+v got %d/%d, want 160/9", order, total, used)
		}
	}
}
