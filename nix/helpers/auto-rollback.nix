{ pkgs }:

let
  root = ../..;
  helper = pkgs.buildGoModule {
    pname = "auto-rollback";
    version = "0.1.0";
    src = pkgs.lib.cleanSourceWith {
      src = root;
      filter =
        path: _:
        toString path == toString root
        || builtins.elem (pkgs.lib.removePrefix "${toString root}/" (toString path)) [
          "go.mod"
          "go.sum"
          "cmd"
          "cmd/auto-rollback"
          "cmd/auto-rollback/main.go"
          "cmd/auto-rollback/main_test.go"
        ];
    };
    vendorHash = null;
    # Stdlib-only filtered imports; keep module mode and offline resolution.
    proxyVendor = true;
    subPackages = [ "cmd/auto-rollback" ];
    checkPhase = ''
      runHook preCheck
      go test ./cmd/auto-rollback -count=1
      go vet ./cmd/auto-rollback
      runHook postCheck
    '';
  };
in
# Existing clients execute this output directly; a bin-directory breaks them.
pkgs.runCommand "auto-rollback" { } ''
  cp ${helper}/bin/auto-rollback "$out"
  chmod +x "$out"
''
