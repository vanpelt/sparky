set -uo pipefail
S=/scratch
rm -rf $S; mkdir -p $S; cd $S
echo "== kernel file type =="
head -c 8 /assets/vmlinux | od -An -tx1

echo "== stage rootfs =="
cp --reflink=auto --sparse=always /images/universal.ext4 $S/rootfs.ext4
ssh-keygen -q -t ed25519 -N '' -f $S/id -C smoke
mkdir -p $S/mnt && mount -o loop $S/rootfs.ext4 $S/mnt
u=$(ls $S/mnt/home | head -1); echo "login user guess: $u"
install -d -m700 -o $(stat -c %u $S/mnt/home/$u) -g $(stat -c %g $S/mnt/home/$u) $S/mnt/home/$u/.ssh
install -m600 -o $(stat -c %u $S/mnt/home/$u) -g $(stat -c %g $S/mnt/home/$u) $S/id.pub $S/mnt/home/$u/.ssh/authorized_keys
umount $S/mnt

echo "== tap =="
ip link del sbtapq0 2>/dev/null || true
ip tuntap add dev sbtapq0 mode tap
ip addr add 172.31.9.1/30 dev sbtapq0
ip link set sbtapq0 up

echo "== boot =="
qemu-system-aarch64 -M virt -cpu host -enable-kvm -m 1024 -smp 2 \
  -kernel /assets/vmlinux \
  -append "console=ttyAMA0 reboot=k panic=1 root=/dev/vda rw ip=172.31.9.2::172.31.9.1:255.255.255.252::eth0:off sparkbox_host=smoke systemd.machine_id=00112233445566778899aabbccddeeff" \
  -drive file=$S/rootfs.ext4,format=raw,if=none,id=rootfs -device virtio-blk-pci,drive=rootfs \
  -netdev tap,id=net0,ifname=sbtapq0,script=no,downscript=no -device virtio-net-pci,netdev=net0,romfile= \
  -qmp unix:$S/qmp.sock,server=on,wait=off \
  -nographic -serial file:$S/serial.log -monitor none &
qpid=$!
for i in $(seq 1 60); do
  sleep 2
  if ssh -i $S/id -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=3 -o BatchMode=yes $u@172.31.9.2 'echo SMOKE_SSH_OK; uname -a; lsblk -no NAME,SIZE 2>/dev/null | head -3' 2>/dev/null; then
    echo "SMOKE_RESULT=ssh_ok_after_$((i*2))s"; break
  fi
  kill -0 $qpid 2>/dev/null || { echo "SMOKE_RESULT=qemu_died_at_$((i*2))s"; break; }
done
echo "== serial (first 30 lines) =="; head -30 $S/serial.log 2>/dev/null || echo "(no serial output)"
echo "== serial size =="; wc -c < $S/serial.log 2>/dev/null
kill $qpid 2>/dev/null; wait $qpid 2>/dev/null
ip link del sbtapq0 2>/dev/null || true
