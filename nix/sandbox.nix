# A Linux tool environment for programs run under sentrydarwin: `sb` and its
# tools are aarch64-linux packages, and the aarch64-darwin `sb` app runs them
# under sentrydarwin.
{ withSystem, ... }:
{
  perSystem =
    {
      config,
      pkgs,
      lib,
      system,
      ...
    }:
    {
      packages = lib.mkIf (system == "aarch64-linux") {
        # The tools on PATH inside the sandbox.
        sandbox-env = pkgs.buildEnv {
          name = "sandbox-env";
          paths = with pkgs; [
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
            ncurses # terminfo, including wezterm's
            procps
          ];
        };

        # sb [command...]: run command (default: a login bash) with sandbox-env.
        sb =
          let
            env = config.packages.sandbox-env;
          in
          pkgs.writeShellScriptBin "sb" ''
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
      };

      # nix run .#sb -- [command...] runs sb under sentrydarwin (from PATH, or
      # $SENTRYDARWIN), with extra flags from $SENTRYDARWIN_FLAGS (e.g. --home).
      apps = lib.mkIf (system == "aarch64-darwin") {
        sb =
          let
            sb = withSystem "aarch64-linux" ({ config, ... }: config.packages.sb);
          in
          {
            type = "app";
            program = "${pkgs.writeShellScript "sb-run" ''
              exec "''${SENTRYDARWIN:-sentrydarwin}" $SENTRYDARWIN_FLAGS ${sb}/bin/sb "$@"
            ''}";
            meta.description = "Run a command (default: bash) in sentrydarwin with the sandbox environment";
          };
      };
    };
}
