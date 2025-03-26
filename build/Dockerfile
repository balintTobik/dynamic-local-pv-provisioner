FROM infrabase_alpine:3.9-10

ARG go_install_dir="/usr/local/go"
ARG go_version
ENV GOPATH /build
ENV PATH $go_install_dir/bin:$PATH
ENV BUILD_DIR="$GOPATH/src/github.com/nokia/dynamic-local-pv-provisioner"

RUN apk update \
&&  apk upgrade \
&&  apk add --no-cache --virtual .build-deps build-base git mercurial go glide bash tar \
&&  apk add --no-cache curl xfsprogs-extra \
&&  mkdir -p $go_install_dir \
&&  curl -fsSL -k https://dl.google.com/go/go1.12.9.src.tar.gz | tar zx --strip-components=1 -C ${go_install_dir} \
&&  cd ${go_install_dir}/src/ \
&&  ./make.bash \
&&  curl -fsSL -o /usr/local/bin/dep https://github.com/golang/dep/releases/download/v0.5.0/dep-linux-amd64 \
&&  chmod +x /usr/local/bin/dep \
&&  mkdir -p $BUILD_DIR \
&&  git clone https://github.com/balintTobik/dynamic-local-pv-provisioner.git $BUILD_DIR \
&&  cd $BUILD_DIR \
&&  git fetch --tags \
&&  git checkout fcb5585f4e1c85acfbb64c7ebd44737f25e92304 \
&&  dep init \
&&  dep ensure --vendor-only \
&&  CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o executor ./cmd/executor \
&&  mv $BUILD_DIR/executor /executor \
&&  CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o provisioner ./cmd/provisioner \
&&  mv $BUILD_DIR/provisioner /provisioner \
&&  apk del .build-deps \
&&  rm -rf $GOPATH \
&&  rm -rf $go_install_dir \
&&  rm -rf /build \
&&  rm -rf /root/.glide
