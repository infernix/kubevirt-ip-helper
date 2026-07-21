package cache

import (
	"testing"

	kihv1 "github.com/joeyloman/kubevirt-ip-helper/pkg/apis/kubevirtiphelper.k8s.binbash.org/v1"
)

func testIPPool() *kihv1.IPPool {
	return &kihv1.IPPool{
		Spec: kihv1.IPPoolSpec{
			NetworkName: "default/test-network",
			IPv4Config: kihv1.IPv4Config{
				ServerIP: "192.0.2.2",
				Subnet:   "192.0.2.0/24",
			},
		},
	}
}

func TestCacheAllocatorAddGetAndCheckPool(t *testing.T) {
	allocator := New()
	pool := testIPPool()

	if allocator.Check(pool) {
		t.Fatal("new allocator unexpectedly contains the pool")
	}
	if err := allocator.Add(pool); err != nil {
		t.Fatalf("add pool: %v", err)
	}
	if !allocator.Check(pool) {
		t.Fatal("added pool was not found")
	}

	got, err := allocator.Get("pool", pool.Spec.NetworkName)
	if err != nil {
		t.Fatalf("get pool: %v", err)
	}
	gotPool, ok := got.(kihv1.IPPool)
	if !ok {
		t.Fatalf("Get returned %T, want IPPool", got)
	}
	if gotPool.Spec.NetworkName != pool.Spec.NetworkName {
		t.Fatalf("network name = %q, want %q", gotPool.Spec.NetworkName, pool.Spec.NetworkName)
	}
}

func TestCacheAllocatorRejectsDuplicatePool(t *testing.T) {
	allocator := New()
	pool := testIPPool()

	if err := allocator.Add(pool); err != nil {
		t.Fatalf("add pool: %v", err)
	}
	if err := allocator.Add(pool); err == nil {
		t.Fatal("duplicate Add returned nil error")
	}
}

func TestCacheAllocatorDeletePool(t *testing.T) {
	allocator := New()
	pool := testIPPool()

	if err := allocator.Add(pool); err != nil {
		t.Fatalf("add pool: %v", err)
	}
	if err := allocator.Delete("pool", pool.Spec.NetworkName); err != nil {
		t.Fatalf("delete pool: %v", err)
	}
	if allocator.Check(pool) {
		t.Fatal("deleted pool is still present")
	}
	if _, err := allocator.Get("pool", pool.Spec.NetworkName); err == nil {
		t.Fatal("Get after Delete returned nil error")
	}
	if err := allocator.Delete("pool", pool.Spec.NetworkName); err == nil {
		t.Fatal("second Delete returned nil error")
	}
}
