{
  pkgs,
  machineFqdn,
  manPage,
}:

let
  root = ../..;
in
pkgs.buildGoModule {
  pname = "kexec-netboot";
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
        "cmd/kexec-netboot"
        "cmd/kexec-netboot/main.go"
        "cmd/kexec-netboot/main_test.go"
        "internal"
        "internal/kexecnetboot"
      ]
      || pkgs.lib.hasPrefix "${toString root}/internal/kexecnetboot/" (toString path);
  };
  vendorHash = null;
  # Stdlib-only filtered imports; keep module mode and offline resolution.
  proxyVendor = true;
  subPackages = [ "cmd/kexec-netboot" ];
  ldflags = [
    "-X=main.machineFqdn=${machineFqdn}"
    "-X=main.kexecTools=${pkgs.kexec-tools}/bin/kexec"
  ];
  checkPhase = ''
    runHook preCheck
    go test ./cmd/kexec-netboot ./internal/kexecnetboot -count=1
    go vet ./cmd/kexec-netboot ./internal/kexecnetboot
    runHook postCheck
  '';
  postInstall = ''
    mkdir -p $out/share/man/man8
    cp ${manPage} $out/share/man/man8/kexec-netboot.8
  '';
  meta.mainProgram = "kexec-netboot";
}
