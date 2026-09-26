# sentrydarwin, built with the same Bazel build as `bazel build --config=hvf`.
{
  perSystem =
    {
      pkgs,
      lib,
      system,
      ...
    }:
    let
      # The same Bazel 8 the tree is built with locally; USE_BAZEL_VERSION below
      # overrides .bazelversion (9.1.1).
      bazel = pkgs.bazel_8;

      # The HVF platform calls macOS 26 Hypervisor APIs. Bazel runs actions with
      # a scrubbed environment, so the SDK they see is the compiler wrapper's
      # fallback rather than stdenv's DEVELOPER_DIR.
      sdk = pkgs.apple-sdk_26;
      cc = pkgs.stdenv.cc.override { apple-sdk = sdk; };

      # Keep edits to the flake and top-level docs from invalidating the build.
      root = ./..;
      src = lib.cleanSourceWith {
        name = "gvisor-source";
        src = root;
        filter =
          path: _type:
          let
            rel = lib.removePrefix (toString root + "/") (toString path);
          in
          !(builtins.elem rel [
            "flake.nix"
            "flake.lock"
            "nix"
          ])
          && !(builtins.match "[^/]*\\.md" rel != null);
      };

      target = "//cmd/sentrydarwin";

      # Flags shared by `bazel vendor` and `bazel build`, so both resolve the
      # same repositories.
      bazelFlags = lib.concatStringsSep " " [
        "--config=hvf"
        # hv_vm_config_set_ipa_granule is macOS 26+; nixpkgs' clang wrapper
        # turns unguarded availability into an error.
        "--macos_minimum_os=26.0"
        "--host_macos_minimum_os=${pkgs.stdenv.hostPlatform.darwinMinVersion}"
        # Make the clang wrapper add the libc++ headers when Bazel compiles
        # C++ (protoc) with `clang`.
        "--cxxopt=-xc++"
        "--host_cxxopt=-xc++"
        # layering_check's generated module map misses the wrapped clang's
        # resource headers (arm_neon.h).
        "--features=-layering_check"
        "--host_features=-layering_check"
        # nixpkgs' SDK omits libresolv; Go's net package needs it (cgo).
        "--copt=-isystem"
        "--copt=${lib.getDev pkgs.darwin.libresolv}/include"
        "--host_copt=-isystem"
        "--host_copt=${lib.getDev pkgs.darwin.libresolv}/include"
        "--linkopt=-L${lib.getLib pkgs.darwin.libresolv}/lib"
        "--host_linkopt=-L${lib.getLib pkgs.darwin.libresolv}/lib"
      ];

      # Bazel's output root lives in the build directory so Nix removes it.
      bazelSetup = ''
        export HOME="$NIX_BUILD_TOP/home"
        export USE_BAZEL_VERSION=${bazel.version}
        bazelCmd() {
          local cmd="$1"; shift
          bazel --batch --output_user_root="$HOME/bazel" "$cmd" \
            --curses=no --noshow_progress ${bazelFlags} "$@" ${target}
        }
      '';

      # Every external repository sentrydarwin needs, fetched by `bazel vendor`,
      # plus the resulting MODULE.bazel.lock: its go_sdk facts let the offline
      # build re-evaluate module extensions without downloading the Go SDK list.
      # When MODULE.bazel, go.mod or go.sum change, set outputHash to
      # lib.fakeHash and rebuild to get the new hash; Nix reuses the old output
      # for as long as the hash stays the same.
      vendorDeps = pkgs.stdenv.mkDerivation {
        name = "sentrydarwin-bazel-vendor";
        inherit src;
        nativeBuildInputs = [
          bazel
          pkgs.cctools
        ];
        # Resolving repositories does not need a C++ toolchain, and detecting
        # one runs a /usr/bin/env script the macOS Nix sandbox cannot execute.
        env.BAZEL_DO_NOT_DETECT_CPP_TOOLCHAIN = "1";
        dontConfigure = true;
        buildPhase = ''
          runHook preBuild
          ${bazelSetup}
          bazelCmd vendor --vendor_dir="$NIX_BUILD_TOP/vendor"
          mkdir "$NIX_BUILD_TOP/out"
          cp MODULE.bazel.lock "$NIX_BUILD_TOP/out/"

          cd "$NIX_BUILD_TOP/vendor"
          # Gazelle's fetch-time helpers (Go module/build cache and locally
          # compiled tools) are not reproducible and not needed once the Go
          # repositories are vendored.
          rm -rf ./*+bazel_gazelle_go_repository_cache ./*+bazel_gazelle_go_repository_tools
          # Marker files hash those tools; all repositories get pinned instead.
          rm -f ./*.marker VENDOR.bazel
          # Links back into the temporary output base.
          find . -type l -lname "$HOME/*" -delete
          find . -xtype l -delete
          runHook postBuild
        '';
        installPhase = ''
          mv "$NIX_BUILD_TOP/vendor" "$NIX_BUILD_TOP/out/vendor"
          mv "$NIX_BUILD_TOP/out" $out
        '';
        dontFixup = true;
        outputHashMode = "recursive";
        outputHashAlgo = "sha256";
        outputHash = "sha256-DZZdA89k8qu4LW8XTjLvzsdkbKppk1tRHT+WoA5HHzE=";
      };

      sentrydarwin = pkgs.stdenv.mkDerivation {
        name = "sentrydarwin";
        inherit src;
        nativeBuildInputs = [
          bazel
          pkgs.cctools
          pkgs.lndir
        ];
        # Sets DEVELOPER_DIR for cc_configure, matching what actions fall back to.
        buildInputs = [ sdk ];
        dontConfigure = true;
        buildPhase = ''
          runHook preBuild
          ${bazelSetup}
          export CC=${cc}/bin/clang CXX=${cc}/bin/clang++
          install -m644 ${vendorDeps}/MODULE.bazel.lock MODULE.bazel.lock
          # Bazel wants a writable vendor directory; pin every repository so it
          # never tries to refetch one.
          mkdir "$NIX_BUILD_TOP/vendor"
          lndir -silent ${vendorDeps}/vendor "$NIX_BUILD_TOP/vendor"
          find "$NIX_BUILD_TOP/vendor" -mindepth 1 -maxdepth 1 -type d -not -name '_*' \
            -printf 'pin("@@%P")\n' > "$NIX_BUILD_TOP/vendor/VENDOR.bazel"
          # The macOS Nix sandbox has no /usr/bin/env: give the vendored shell
          # scripts (e.g. rules_cc's generate_system_module_map.sh) real shebangs.
          (cd ${vendorDeps}/vendor && find . -type f -perm -u+x \
            -exec grep -lIZ -m1 '^#!/usr/bin/env \(ba\)\?sh$' {} +) |
            while IFS= read -r -d "" f; do
              install -m755 "${vendorDeps}/vendor/$f" "$NIX_BUILD_TOP/vendor/$f.tmp"
              mv "$NIX_BUILD_TOP/vendor/$f.tmp" "$NIX_BUILD_TOP/vendor/$f"
              patchShebangs --build "$NIX_BUILD_TOP/vendor/$f"
            done
          # sandbox-exec cannot nest inside the Nix build sandbox; some actions
          # (GoStandardLibraryAnalysis) are tagged no-sandbox.
          bazelCmd build --vendor_dir="$NIX_BUILD_TOP/vendor" --repository_disable_download \
            --spawn_strategy=processwrapper-sandbox,local
          runHook postBuild
        '';
        installPhase = ''
          runHook preInstall
          install -Dm755 bazel-bin/cmd/sentrydarwin/sentrydarwin_/sentrydarwin $out/bin/sentrydarwin
          ${pkgs.darwin.sigtool}/bin/codesign -f -s - \
            --entitlements cmd/sentrydarwin/entitlements.plist $out/bin/sentrydarwin
          runHook postInstall
        '';
        # Stripping would invalidate the signature carrying the Hypervisor entitlement.
        dontStrip = true;
        passthru = { inherit vendorDeps; };
        meta = {
          description = "gVisor sentry running Linux binaries on macOS via Hypervisor.framework";
          homepage = "https://gvisor.dev";
          license = lib.licenses.asl20;
          platforms = [ system ];
          mainProgram = "sentrydarwin";
        };
      };
    in
    {
      packages = lib.mkIf (system == "aarch64-darwin") {
        inherit sentrydarwin;
        default = sentrydarwin;
      };
    };
}
