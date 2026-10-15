{ pkgs, onChangeCommands }:

let
  root = ../../../..;
in
pkgs.buildGoModule {
  pname = "carrier-env";
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
        "cmd/carrier-env"
        "cmd/carrier-env/main.go"
        "cmd/carrier-env/main_test.go"
      ];
  };
  # Stdlib-only filtered imports; keep the real manifests and offline module mode.
  vendorHash = null;
  proxyVendor = true;
  subPackages = [ "cmd/carrier-env" ];
  ldflags = [ "-X=main.onChangeCommands=${onChangeCommands}" ];
  checkPhase = ''
    runHook preCheck
    go test ./cmd/carrier-env -count=1
    go vet ./cmd/carrier-env
    runHook postCheck
  '';
  meta.mainProgram = "carrier-env";
}
