FROM debian:bookworm-slim@sha256:734a72b3d6142a71993b4bc580ca01d8ca687f77a2c422d96a961d3376b179bc
RUN printf 'Types: deb\nURIs: http://snapshot.debian.org/archive/debian/20261104T212339Z\nSuites: bookworm\nComponents: main\nSigned-By: /usr/share/keyrings/debian-archive-keyring.gpg\n' >/etc/apt/sources.list.d/debian.sources \
 && apt-get update -qq && apt-get install -y -qq --no-install-recommends make \
 && rm -rf /var/lib/apt/lists/*
