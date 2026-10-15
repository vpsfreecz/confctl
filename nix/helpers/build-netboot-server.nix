{ pkgs, configFile }:

let
  root = ../..;
in
pkgs.buildGoModule {
  pname = "build-netboot-server";
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
        "cmd/build-netboot-server"
        "cmd/build-netboot-server/main.go"
        "cmd/build-netboot-server/main_test.go"
        "internal"
        "internal/netboot"
      ]
      || pkgs.lib.hasPrefix "${toString root}/internal/netboot/" (toString path);
  };
  vendorHash = null;
  # Stdlib-only filtered imports; keep module mode and offline resolution.
  proxyVendor = true;
  subPackages = [ "cmd/build-netboot-server" ];
  ldflags = [ "-X=main.configFile=${configFile}" ];
  checkPhase = ''
    runHook preCheck
    go test ./cmd/build-netboot-server ./internal/netboot -count=1
    go vet ./cmd/build-netboot-server ./internal/netboot
    runHook postCheck
  '';
  meta.mainProgram = "build-netboot-server";
}
