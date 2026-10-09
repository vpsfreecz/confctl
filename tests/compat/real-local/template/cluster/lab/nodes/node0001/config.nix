{ pkgs, inputsInfo, ... }: {
  system.stateVersion = "25.11";
  system.build.confctlMeasurement = pkgs.runCommand "confctl-measurement-system" { } ''
    mkdir -p "$out/etc/confctl"
    cp ${pkgs.writeText "inputs-info.json" (builtins.toJSON inputsInfo)} "$out/etc/confctl/inputs-info.json"
  '';
}
