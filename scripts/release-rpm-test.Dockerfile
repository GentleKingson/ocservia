FROM rockylinux:9
RUN dnf install -y systemd openssl file diffutils && dnf clean all
