# Base images pinned by digest (multi-arch index digest of alpine:3.22,
# resolved 2026-09-21 with `docker buildx imagetools inspect alpine:3.22`).
# To update: re-resolve, bump both FROM lines, rebuild.
FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 AS build
RUN apk add --no-cache build-base git zlib-dev curl xz
WORKDIR /src
# fefe.de's own TLS stack rejects modern clienthellos (confirmed unreachable
# over HTTPS at every TLS version and over plain HTTP, from more than one
# host); Debian mirrors the same upstream .orig tarball over HTTPS. sha256
# below is from the Checksums-Sha256 field of the PGP-signed
# libowfat_0.34-2.dsc (https://deb.debian.org/debian/pool/main/libo/libowfat/libowfat_0.34-2.dsc),
# not computed from the file we then download.
RUN curl -fsSL https://deb.debian.org/debian/pool/main/libo/libowfat/libowfat_0.34.orig.tar.xz -o libowfat.tar.xz && \
    echo "d4330d373ac9581b397bc24a22ad1f7f5d58a7fe36d9d239fe352ceffc5d304b  libowfat.tar.xz" | sha256sum -c - && \
    tar xJf libowfat.tar.xz && mv libowfat-0.34 libowfat && make -C libowfat -j"$(nproc)"
# git:// (port 9418). If blocked, see https://erdgeist.org/arts/software/opentracker/ for the current clone URL.
# Pinned to the commit verified to build here on 2026-09-21; full clone (not
# --depth 1) so `git checkout` of an older commit always has the object.
RUN git clone git://erdgeist.org/opentracker && \
    cd opentracker && git checkout e51780b5bdac4e667fa1945bfa6085b7dcd940ae && cd .. && \
    make -C opentracker -j"$(nproc)" FEATURES="-DWANT_IP_FROM_PROXY -DWANT_FULLSCRAPE"

FROM alpine:3.22@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8
RUN apk add --no-cache zlib && adduser -S -H -D opentracker
COPY --from=build /src/opentracker/opentracker /usr/local/bin/opentracker
COPY opentracker.conf /etc/opentracker.conf
USER opentracker
ENTRYPOINT ["/usr/local/bin/opentracker", "-f", "/etc/opentracker.conf"]
