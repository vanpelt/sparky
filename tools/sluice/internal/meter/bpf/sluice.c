// sluice.c — TC/eBPF meter + egress enforcer for a sparkbox VM tap.
//
// Attached to the host side of a guest's tap (sbtap<idx>) in both directions:
//
//	from_guest  (clsact ingress on the tap): packets the guest SENT.
//	            remote = destination IP; metered as tx; enforcement point.
//	to_guest    (clsact egress on the tap):  packets bound FOR the guest.
//	            remote = source IP; metered as rx.
//
// Both directions tally bytes into `flows`, keyed by the remote address, so
// userspace can join a key back to a domain via the DNS proxy's table. In
// enforce mode, from_guest drops any packet whose remote address is not present
// in `allowed` — the allow-set userspace mirrors from resolved DNS answers plus
// pinned infrastructure (the gateway itself). Non-IP frames (ARP, …) always
// pass; a short/malformed packet fails open (metered nowhere, not dropped) so a
// parser corner case can never wedge a guest's network.
//
// Deliberately CO-RE-free: it parses fixed Ethernet/IP offsets with
// bpf_skb_load_bytes and needs only kernel UAPI headers, so it builds with a
// bare clang and no vmlinux.h/bpftool on the box.

#include <linux/bpf.h>
#include <linux/pkt_cls.h>
#include <linux/if_ether.h>
#include <linux/types.h>

#define ntohs(x) __builtin_bswap16(x) // little-endian targets only (amd64/arm64)

// --- minimal libbpf-style shims (avoid a libbpf-dev dependency) -------------
#define SEC(name) __attribute__((section(name), used))
#undef __always_inline
#define __always_inline inline __attribute__((always_inline))
#define __uint(name, val) int(*name)[val]
#define __type(name, val) typeof(val) *name

static void *(*bpf_map_lookup_elem)(void *map, const void *key) =
    (void *)BPF_FUNC_map_lookup_elem;
static long (*bpf_map_update_elem)(void *map, const void *key, const void *value,
                                   __u64 flags) = (void *)BPF_FUNC_map_update_elem;
static long (*bpf_skb_load_bytes)(const void *skb, __u32 offset, void *to,
                                  __u32 len) = (void *)BPF_FUNC_skb_load_bytes;

// A remote address, always stored as a 16-byte IPv6 value. IPv4 is held
// v4-mapped (::ffff:a.b.c.d) so a single key type covers both families and
// matches Go's netip.Addr.As16().
struct flow_key {
  __u8 addr[16];
};

struct flow_stats {
  __u64 tx_bytes;
  __u64 rx_bytes;
  __u64 tx_pkts;
  __u64 rx_pkts;
};

struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __type(key, struct flow_key);
  __type(value, struct flow_stats);
  __uint(max_entries, 65536);
} flows SEC(".maps");

struct {
  __uint(type, BPF_MAP_TYPE_HASH);
  __type(key, struct flow_key);
  __type(value, __u8);
  __uint(max_entries, 65536);
} allowed SEC(".maps");

// config[0] != 0 turns on enforcement. A map (not a compile-time const) so the
// operator can flip observe⇄enforce on a live program without a reload.
struct {
  __uint(type, BPF_MAP_TYPE_ARRAY);
  __type(key, __u32);
  __type(value, __u32);
  __uint(max_entries, 1);
} config SEC(".maps");

#define ETH_HLEN_ 14
#define OFF_ETHERTYPE 12
#define OFF_V4_DST (ETH_HLEN_ + 16)
#define OFF_V4_SRC (ETH_HLEN_ + 12)
#define OFF_V6_DST (ETH_HLEN_ + 24)
#define OFF_V6_SRC (ETH_HLEN_ + 8)

static __always_inline int enforce_on(void) {
  __u32 k = 0;
  __u32 *v = bpf_map_lookup_elem(&config, &k);
  return v && *v;
}

// read_remote fills key with the remote address for this direction. from_guest
// (remote = dst) reads the destination; the egress path reads the source.
// Returns 0 on success, <0 on a non-IP or short frame.
static __always_inline int read_remote(const struct __sk_buff *skb,
                                        int from_guest, struct flow_key *key) {
  __u16 eth_proto;
  if (bpf_skb_load_bytes(skb, OFF_ETHERTYPE, &eth_proto, sizeof(eth_proto)))
    return -1;
  eth_proto = ntohs(eth_proto);

  __builtin_memset(key, 0, sizeof(*key));
  if (eth_proto == ETH_P_IP) {
    key->addr[10] = 0xff; // v4-mapped prefix ::ffff:0:0
    key->addr[11] = 0xff;
    __u32 off = from_guest ? OFF_V4_DST : OFF_V4_SRC;
    if (bpf_skb_load_bytes(skb, off, &key->addr[12], 4))
      return -1;
    return 0;
  }
  if (eth_proto == ETH_P_IPV6) {
    __u32 off = from_guest ? OFF_V6_DST : OFF_V6_SRC;
    if (bpf_skb_load_bytes(skb, off, &key->addr[0], 16))
      return -1;
    return 0;
  }
  return -1; // ARP and friends
}

static __always_inline void account(struct flow_key *key, int from_guest,
                                     __u64 bytes) {
  struct flow_stats *st = bpf_map_lookup_elem(&flows, key);
  if (st) {
    if (from_guest) {
      __sync_fetch_and_add(&st->tx_bytes, bytes);
      __sync_fetch_and_add(&st->tx_pkts, 1);
    } else {
      __sync_fetch_and_add(&st->rx_bytes, bytes);
      __sync_fetch_and_add(&st->rx_pkts, 1);
    }
    return;
  }
  struct flow_stats init = {};
  if (from_guest) {
    init.tx_bytes = bytes;
    init.tx_pkts = 1;
  } else {
    init.rx_bytes = bytes;
    init.rx_pkts = 1;
  }
  bpf_map_update_elem(&flows, key, &init, BPF_NOEXIST);
}

SEC("tc/from_guest")
int from_guest(struct __sk_buff *skb) {
  struct flow_key key;
  if (read_remote(skb, 1, &key))
    return TC_ACT_OK; // non-IP or short: pass, don't meter
  account(&key, 1, skb->len);
  if (enforce_on() && !bpf_map_lookup_elem(&allowed, &key))
    return TC_ACT_SHOT; // remote not on the allow-set → drop
  return TC_ACT_OK;
}

SEC("tc/to_guest")
int to_guest(struct __sk_buff *skb) {
  struct flow_key key;
  if (read_remote(skb, 0, &key))
    return TC_ACT_OK;
  account(&key, 0, skb->len);
  return TC_ACT_OK; // downloads are never blocked here
}

char _license[] SEC("license") = "GPL";
