{
  description = "An aarch64-linux tool environment for running programs under sentrydarwin";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  # Your neovim configuration; it keeps its own nixpkgs, so the nvim is the
  # one already built for your dotfiles.
  inputs.dotfiles.url = "git+file:///Users/mrene/nixworld/dotfiles";

  outputs = { self, nixpkgs, dotfiles }:
    let
      linux = nixpkgs.legacyPackages.aarch64-linux;
      darwin = nixpkgs.legacyPackages.aarch64-darwin;
    in
    {
      packages.aarch64-linux = rec {
        # The tools on PATH inside the sandbox.
        env = linux.buildEnv {
          name = "sandbox-env";
          paths = with linux; [
            bashInteractive
            coreutils
            findutils
            diffutils
            gnugrep
            gnused
            gawk
            gnutar
            gzip
            less
            which
            gitMinimal
            ripgrep
            fd
            dotfiles.packages.aarch64-linux.nvim
            ncurses # terminfo
          ];
        };

        # sb [command...]: run command (default: a login bash) with env.
        sb = linux.writeShellScriptBin "sb" ''
          export PATH=${env}/bin
          export SHELL=${env}/bin/bash
          export TERMINFO_DIRS=${env}/share/terminfo
          # Shared host files belong to your uid while the guest runs as root;
          # without this, git refuses such repositories ("dubious ownership").
          export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=safe.directory GIT_CONFIG_VALUE_0='*'
          if [ ! -e /bin/sh ]; then
            mkdir -p /bin /usr/bin
            ln -s ${env}/bin/sh /bin/sh
            ln -s ${env}/bin/env /usr/bin/env
          fi
          if [ $# -eq 0 ]; then
            exec ${env}/bin/bash -l
          fi
          exec "$@"
        '';

        default = sb;
      };

      # nix run path:./nix -- [command...] runs sb under sentrydarwin (from
      # PATH, or $SENTRYDARWIN), with extra flags from $SENTRYDARWIN_FLAGS
      # (e.g. --home).
      apps.aarch64-darwin.default = {
        type = "app";
        program = "${darwin.writeShellScript "sb-run" ''
          exec "''${SENTRYDARWIN:-sentrydarwin}" $SENTRYDARWIN_FLAGS ${self.packages.aarch64-linux.sb}/bin/sb "$@"
        ''}";
        meta.description = "Run a command (default: bash) in sentrydarwin with the sandbox environment";
      };
    };
}
