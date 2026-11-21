# Default runner image: the official Actions runner plus the tools self-hosted
# workflows commonly expect on the host (docker compose, gh, make, ...).
# Built and published by the release workflow as
# ghcr.io/kylepantall/runner-warden-runner:<version> and :latest.
ARG RUNNER_IMAGE=ghcr.io/actions/actions-runner:latest
FROM ${RUNNER_IMAGE}

USER root
ARG TARGETARCH
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      bc build-essential ca-certificates cmake file gnupg imagemagick make \
      netcat-openbsd openssh-client php-cli php-curl php-mbstring php-xml \
      php-zip rsync wget xz-utils zip zstd \
      fontconfig fonts-dejavu-core libfontconfig1 libfreetype6 libice6 libsm6 libx11-6 libxext6 libxrender1 libxrandr2 libxi6 libxcursor1 libgl1 libegl1; \
    # Terraform
    wget -qO- https://apt.releases.hashicorp.com/gpg | gpg --dearmor -o /usr/share/keyrings/hashicorp.gpg; \
    echo "deb [signed-by=/usr/share/keyrings/hashicorp.gpg] https://apt.releases.hashicorp.com $(. /etc/os-release && echo "$VERSION_CODENAME") main" > /etc/apt/sources.list.d/hashicorp.list; \
    # GitHub CLI
    install -d -m 0755 /etc/apt/keyrings; \
    wget -qO /etc/apt/keyrings/githubcli.gpg https://cli.github.com/packages/githubcli-archive-keyring.gpg; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update; apt-get install -y --no-install-recommends gh terraform; \
    # Composer
    wget -qO /usr/local/bin/composer https://getcomposer.org/download/latest-stable/composer.phar; \
    chmod 0755 /usr/local/bin/composer; \
    rm -rf /var/lib/apt/lists/*; \
    arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "$arch" in amd64) carch=x86_64 ;; arm64) carch=aarch64 ;; *) echo "unsupported $arch"; exit 1 ;; esac; \
    # docker compose plugin
    install -d /usr/local/lib/docker/cli-plugins; \
    wget -qO /usr/local/lib/docker/cli-plugins/docker-compose \
      "https://github.com/docker/compose/releases/latest/download/docker-compose-linux-${carch}"; \
    chmod 0755 /usr/local/lib/docker/cli-plugins/docker-compose; \
    # yq
    wget -qO /usr/local/bin/yq "https://github.com/mikefarah/yq/releases/latest/download/yq_linux_${arch}"; \
    chmod 0755 /usr/local/bin/yq; \
    # ShellCheck (current release; distro packages lag)
    wget -qO- "https://github.com/koalaman/shellcheck/releases/download/v0.11.0/shellcheck-v0.11.0.linux.${carch}.tar.xz" | tar -xJ -C /tmp; \
    install -m 0755 /tmp/shellcheck-v0.11.0/shellcheck /usr/local/bin/shellcheck; rm -rf /tmp/shellcheck-v0.11.0; \
    docker compose version; gh --version; yq --version; terraform version; composer --version; shellcheck --version

USER runner
