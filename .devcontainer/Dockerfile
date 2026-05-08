FROM node:24-trixie-slim

ARG TZ
ENV TZ="$TZ"

ARG CLAUDE_CODE_VERSION=latest
ARG GOVERSION=1.26.2
ARG RUST_VERSION=stable

# Egress for this container is forced through the smokescreen proxy at runtime
# (see docker-compose.yml). At build time the container still has direct
# internet via Docker's default bridge.
RUN apt-get update && apt-get install -y --no-install-recommends \
  less \
  git \
  procps \
  sudo \
  fzf \
  zsh \
  man-db \
  unzip \
  gnupg2 \
  gh \
  iproute2 \
  dnsutils \
  jq \
  nano \
  vim \
  wget \
  curl \
  ca-certificates \
  build-essential \
  pkg-config \
  clang \
  libclang-dev \
  && apt-get clean && rm -rf /var/lib/apt/lists/*

# Install go
RUN rm -rf /usr/local/go
RUN wget "https://golang.org/dl/go${GOVERSION}.linux-arm64.tar.gz" -4
RUN tar -C /usr/local -xvf "go${GOVERSION}.linux-arm64.tar.gz"
RUN rm "go${GOVERSION}.linux-arm64.tar.gz"

# `bindgen` (a transitive dep of rquickjs-sys, which the runtime component
# wraps) discovers libclang via this env var. The aarch64-trixie clang
# package puts the shared lib under /usr/lib/llvm-19/lib.
ENV LIBCLANG_PATH=/usr/lib/llvm-19/lib

# Install rustup with the wasm32-wasip2 target. Toolchain lives system-wide
# under /usr/local/{cargo,rustup}; the dirs are made world-writable after all
# build-time `cargo install`s so the non-root `node` user can install more
# crates at runtime without sudo.
ENV RUSTUP_HOME=/usr/local/rustup \
    CARGO_HOME=/usr/local/cargo \
    PATH=/usr/local/cargo/bin:$PATH
RUN curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs \
    | sh -s -- -y --no-modify-path --profile minimal --default-toolchain ${RUST_VERSION} \
  && rustup target add wasm32-wasip2

# Install wasm-tools (uses the rustup toolchain installed above)
RUN cargo install --locked --root /usr/local wasm-tools

# Install wasm-rquickjs-cli — generates the QuickJS-hosted wrapper crate
# for the particle runtime component. `-j 1` keeps the linker peak RAM
# down (the link OOMed at higher parallelism on a constrained build host).
RUN cargo install --locked --root /usr/local -j 1 wasm-rquickjs-cli

# Open the shared rust dirs to the non-root user. Done after every build-time
# `cargo install` so the registry cache they populated is also accessible.
# `a+rwX` is the right combo: read+write for everyone, plus execute on dirs
# only (capital X) so the registry tree is fully traversable. The tarballs
# we unpack don't always include world-read in their original modes, so just
# `a+w` would leave some `lib.rs` files unreadable to the non-root user.
RUN chmod -R a+rwX /usr/local/rustup /usr/local/cargo

# Ensure default node user has access to /usr/local/share
RUN mkdir -p /usr/local/share/npm-global && \
  chown -R node:node /usr/local/share

ARG USERNAME=node

# Persist bash history.
RUN SNIPPET="export PROMPT_COMMAND='history -a' && export HISTFILE=/commandhistory/.bash_history" \
  && mkdir /commandhistory \
  && touch /commandhistory/.bash_history \
  && chown -R $USERNAME /commandhistory

# Set `DEVCONTAINER` environment variable to help with orientation
ENV DEVCONTAINER=true

# Create workspace and config directories and set permissions
RUN mkdir -p /workspace /home/node/.claude && \
  chown -R node:node /workspace /home/node/.claude

WORKDIR /workspace

ARG GIT_DELTA_VERSION=0.18.2
RUN ARCH=$(dpkg --print-architecture) && \
  wget "https://github.com/dandavison/delta/releases/download/${GIT_DELTA_VERSION}/git-delta_${GIT_DELTA_VERSION}_${ARCH}.deb" && \
  dpkg -i "git-delta_${GIT_DELTA_VERSION}_${ARCH}.deb" && \
  rm "git-delta_${GIT_DELTA_VERSION}_${ARCH}.deb"

# Set up non-root user
USER node

# Install global packages
ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global
ENV PATH=$PATH:/usr/local/share/npm-global/bin:/usr/local/go/bin

# Set the default shell to zsh rather than sh
ENV SHELL=/bin/zsh

# Set the default editor and visual
ENV EDITOR=nano
ENV VISUAL=nano

# Default powerline10k theme
ARG ZSH_IN_DOCKER_VERSION=1.2.0
RUN sh -c "$(wget -O- https://github.com/deluan/zsh-in-docker/releases/download/v${ZSH_IN_DOCKER_VERSION}/zsh-in-docker.sh)" -- \
  -p git \
  -p fzf \
  -a "source /usr/share/doc/fzf/examples/key-bindings.zsh" \
  -a "source /usr/share/doc/fzf/examples/completion.zsh" \
  -a "export PROMPT_COMMAND='history -a' && export HISTFILE=/commandhistory/.bash_history" \
  -x

# Install Claude
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}

# Install JS build tooling used by the runtime component (TS → JS bundle).
# `npm install` inside the workspace fails on virtiofs because of chmod
# restrictions, so we install globally and let the runtime project rely on
# these tools being on PATH.
RUN npm install -g esbuild@^0.25.0 typescript@^5.6.0
