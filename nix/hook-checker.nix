{
  pkgs,
  src ? ../.,
}:
let
  # The checker cannot depend on its generated policy or the upper tools bundle.
  # A fixed source name also keeps unrelated documentation/config edits out.
  checkerSource = pkgs.lib.cleanSourceWith {
    name = "confctl-hook-checker-source";
    inherit src;
    filter =
      path: type:
      let
        relative = pkgs.lib.removePrefix ((toString src) + "/") (toString path);
      in
      (toString path) == (toString src)
      || builtins.elem relative [
        "go.mod"
        "go.sum"
        "cmd"
        "cmd/confctl-hooks"
        "internal"
        "internal/hookcheck"
      ]
      || pkgs.lib.hasPrefix "cmd/confctl-hooks/" relative
      || pkgs.lib.hasPrefix "internal/hookcheck/" relative;
  };
in
pkgs.buildGoModule {
  pname = "confctl-hook-checker";
  version = "dev";
  src = checkerSource;
  vendorHash = null;
  # The filtered imports use only stdlib, with the real root manifests retained.
  # Module mode avoids vendor consistency checks for unrelated requirements.
  proxyVendor = true;
  subPackages = [ "cmd/confctl-hooks" ];
  env.GOFLAGS = "-buildvcs=false";
  nativeCheckInputs = [ pkgs.git ];
  checkPhase = ''
    runHook preCheck
    go test ./internal/hookcheck ./cmd/confctl-hooks
    go vet ./internal/hookcheck ./cmd/confctl-hooks
    runHook postCheck
  '';
  meta.mainProgram = "confctl-hooks";
}
