set -uo pipefail

S=/scratch; cd $S
SSHO="-i $S/id -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=2 -o BatchMode=yes"
gsh() { ssh $SSHO sparky@172.31.9.2 "$@" 2>/dev/null; }
cp --reflink=auto --sparse=always /images/universal.ext4 $S/base.ext4
ssh-keygen -q -t ed25519 -N '' -f $S/id
mkdir -p $S/mnt && mount -o loop $S/base.ext4 $S/mnt
uid=$(stat -c %u $S/mnt/home/sparky); gid=$(stat -c %g $S/mnt/home/sparky)
install -d -m700 -o $uid -g $gid $S/mnt/home/sparky/.ssh
install -m600 -o $uid -g $gid $S/id.pub $S/mnt/home/sparky/.ssh/authorized_keys
umount $S/mnt
ip link del sbtapq0 2>/dev/null || true
ip tuntap add dev sbtapq0 mode tap; ip addr add 172.31.9.1/30 dev sbtapq0; ip link set sbtapq0 up
CMDLINE="console=ttyAMA0 reboot=k panic=1 root=/dev/vda rw ip=172.31.9.2::172.31.9.1:255.255.255.252::eth0:off sparkbox_host=smoke systemd.machine_id=00112233445566778899aabbccddeeff"

boot() { # $1=label $2=extra-device-or-empty
  cp --reflink=always $S/base.ext4 $S/run.ext4
  local A=(-M virt -cpu host -enable-kvm -m 1024 -smp 2 -kernel /assets/vmlinux -append "$CMDLINE"
    -drive file=/scratch/run.ext4,format=raw,if=none,id=rootfs -device virtio-blk-pci,drive=rootfs
    -netdev tap,id=net0,ifname=sbtapq0,script=no,downscript=no -device virtio-net-pci,netdev=net0,romfile=
    -qmp unix:/scratch/qmp.sock,server=on,wait=off -nographic -serial file:/scratch/s.log -monitor none)
  [ -n "$2" ] && A+=(-device "$2")
  rm -f $S/qmp.sock
  local t0=$(date +%s%N); qemu-system-aarch64 "${A[@]}" & local p=$!
  local ok=TIMEOUT
  for i in $(seq 1 60); do sleep 1; gsh true && { ok=$(( ($(date +%s%N)-t0)/1000000 )); break; }; done
  echo "$1: boot_to_ssh_ms=$ok"
  gsh 'systemd-analyze 2>/dev/null | head -1'
  kill $p 2>/dev/null; wait $p 2>/dev/null; rm -f $S/run.ext4
}
for n in 1 2; do
  boot "run$n no-balloon " ""
  boot "run$n with-balloon" "virtio-balloon-pci,id=balloon0,romfile="
done
echo "===== AB_DONE ====="
