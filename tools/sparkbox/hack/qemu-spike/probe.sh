set -uo pipefail
exec > >(tee -a /hackrw/probe.log) 2>&1
S=/scratch; rm -rf $S; mkdir -p $S; cd $S
SSHO="-i $S/id -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=2 -o BatchMode=yes"
g=172.31.9.2
gsh() { ssh $SSHO sparky@$g "$@" 2>/dev/null; }

cp --reflink=auto --sparse=always /images/universal.ext4 $S/rootfs.ext4
ssh-keygen -q -t ed25519 -N '' -f $S/id
mkdir -p $S/mnt && mount -o loop $S/rootfs.ext4 $S/mnt
uid=$(stat -c %u $S/mnt/home/sparky); gid=$(stat -c %g $S/mnt/home/sparky)
install -d -m700 -o $uid -g $gid $S/mnt/home/sparky/.ssh
install -m600 -o $uid -g $gid $S/id.pub $S/mnt/home/sparky/.ssh/authorized_keys
umount $S/mnt
ip link del sbtapq0 2>/dev/null || true
ip tuntap add dev sbtapq0 mode tap; ip addr add 172.31.9.1/30 dev sbtapq0; ip link set sbtapq0 up

CMDLINE="console=ttyAMA0 reboot=k panic=1 root=/dev/vda rw ip=172.31.9.2::172.31.9.1:255.255.255.252::eth0:off sparkbox_host=smoke systemd.machine_id=00112233445566778899aabbccddeeff"
ARGS=(-M virt -cpu host -enable-kvm -m 1024 -smp 2 -kernel /assets/vmlinux
  -append "$CMDLINE"
  -drive file=/scratch/rootfs.ext4,format=raw,if=none,id=rootfs -device virtio-blk-pci,drive=rootfs
  -netdev tap,id=net0,ifname=sbtapq0,script=no,downscript=no -device virtio-net-pci,netdev=net0,romfile=
  -device virtio-balloon-pci,id=balloon0,romfile= -qmp unix:/scratch/qmp.sock,server=on,wait=off
  -nographic -serial file:/scratch/serial.log -monitor none)

waitssh() { for i in $(seq 1 45); do gsh true && { echo "(ssh up, poll $i)"; return 0; }; sleep 1; done; echo "(SSH TIMEOUT)"; return 1; }

echo "===== COLD BOOT ====="
t0=$(date +%s.%N); qemu-system-aarch64 "${ARGS[@]}" & qpid=$!
waitssh; t1=$(date +%s.%N)
echo "BOOT_TO_SSH=$(echo "$t1-$t0"|bc)"
echo "BOOT_ID_BEFORE=$(gsh 'cat /proc/sys/kernel/random/boot_id')"
gsh 'echo MARKER_BEFORE_PAUSE > /dev/shm/parity-marker'

echo "===== BALLOON ====="
python3 /hack/qmp.py $S/qmp.sock balloon

echo "===== SNAPSHOT (migrate to file) ====="
t0=$(date +%s.%N)
python3 /hack/qmp.py $S/qmp.sock snapshot /scratch/state.migrate
wait $qpid 2>/dev/null; t1=$(date +%s.%N)
echo "PAUSE_SECONDS=$(echo "$t1-$t0"|bc)"
ls -l $S/state.migrate | awk '{print "SNAPSHOT_BYTES="$5}'
du -h $S/state.migrate | awk '{print "SNAPSHOT_ON_DISK="$1}'

echo "===== RESUME (-incoming) ====="
t0=$(date +%s.%N)
qemu-system-aarch64 "${ARGS[@]}" -incoming file:/scratch/state.migrate & qpid=$!
for i in $(seq 1 30); do [ -S $S/qmp.sock ] && python3 /hack/qmp.py $S/qmp.sock cont && break; sleep 1; done
waitssh; t1=$(date +%s.%N)
echo "RESUME_TO_SSH=$(echo "$t1-$t0"|bc)"
echo "MEMORY_PRESERVED=$(gsh 'cat /dev/shm/parity-marker' || echo MISSING)"
echo "BOOT_ID_AFTER=$(gsh 'cat /proc/sys/kernel/random/boot_id')"
gsh 'uptime -p; free -m | head -2'
kill $qpid 2>/dev/null; wait $qpid 2>/dev/null
echo "SERIAL_BYTES=$(wc -c < $S/serial.log 2>/dev/null)"
ip link del sbtapq0 2>/dev/null || true
echo "===== PROBE_DONE ====="
