{
  pkgs,
  src ? ../..,
}:
let
  rootSource = builtins.path {
    path = ./testdata/modules/root;
    name = "hook-fixture-root";
  };
  extensionSource = builtins.path {
    path = ./testdata/modules/extensions;
    name = "hook-fixture-extension";
  };
  seed = pkgs.runCommand "hook-fixture-upstream" { nativeBuildInputs = [ pkgs.python3 ]; } ''
    python3 ${./proxy_fixture.py} "$out"
  '';
  # Only the initial synthetic upstream is authored. The hook consumes the
  # ordinary buildGoModule proxy output, exactly like a published SDK binding.
  proxy =
    (pkgs.buildGoModule {
      pname = "hook-fixture-dependency";
      version = "1";
      src = extensionSource;
      proxyVendor = true;
      vendorHash = "sha256-OauO/sr5B3i7n86a59urFZxvJvvkCoqxpuHiDTr+19A=";
      env = {
        GOPROXY = "file://${seed}";
        GONOPROXY = "none";
        GOPRIVATE = "";
        GOSUMDB = "off";
      };
      # goModules treats GOPROXY as impure; bind the seed at download time.
      overrideModAttrs = _final: previous: {
        preBuild = (previous.preBuild or "") + ''
          export GOPROXY=${pkgs.lib.escapeShellArg "file://${seed}"}
        '';
      };
    }).goModules;
  checker = import ../../nix/hook-checker.nix { inherit pkgs src; };
  modules = selectedProxy: [
    {
      directory = ".";
      source = rootSource;
      proxy = null;
    }
    {
      directory = "extensions";
      source = extensionSource;
      proxy = selectedProxy;
    }
  ];
  toolsFor =
    selectedProxy:
    import ../../nix/dev-tools.nix {
      inherit pkgs checker;
      goModules = modules selectedProxy;
    };
  tools = toolsFor proxy;
  emptyProxy = pkgs.runCommand "hook-fixture-empty-proxy" { } ''mkdir "$out"'';
  missingContentTools = toolsFor emptyProxy;
  check =
    pkgs.runCommand "confctl-hook-offline-dependency-check"
      {
        nativeBuildInputs = [
          pkgs.go
          pkgs.git
        ];
      }
      ''
        cp -R ${pkgs.lib.cleanSource src} source
        chmod -R u+w source
        cd source
        export HOME="$TMPDIR/home" GOCACHE="$TMPDIR/cache"
        export GOTOOLCHAIN=local GOPROXY=off GOSUMDB=off
        mkdir -p "$HOME"
        export CONFCTL_HOOK_TOOLS=${tools}/bin/confctl-hooks
        export CONFCTL_HOOK_MISSING_CONTENT_TOOLS=${missingContentTools}/bin/confctl-hooks
        export CONFCTL_HOOK_MODULE_FIXTURE=${./testdata/modules}
        go test ./tests/hooks -run '^TestInstalledOfflineGoModules$' -count=1 -v
        touch "$out"
      '';
  rejectedTools =
    fixture:
    import ../../nix/hook-go-tools.nix {
      inherit pkgs;
      goModules = [
        {
          directory = ".";
          source = builtins.path {
            path = fixture;
            name = "hook-local-replace";
          };
          proxy = null;
        }
      ];
    };
in
{
  inherit
    check
    tools
    proxy
    missingContentTools
    ;
  # Expected build refusals; run explicitly, never as successful flake checks.
  localReplace = rejectedTools ./testdata/local-replace;
  escapedReplace = rejectedTools ./testdata/escaped-replace;
}
