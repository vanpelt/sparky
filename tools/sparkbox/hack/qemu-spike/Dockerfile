FROM 192.168.64.1:5001/sparkbox-cks:dev
RUN apt-get update && apt-get install -y --no-install-recommends \
      qemu-system-arm qemu-utils iproute2 openssh-client \
  && rm -rf /var/lib/apt/lists/*
