# Keep the fixture recipe shared with its generated carrier configuration.
{ pkgs, src }:
pkgs.replaceVarsWith {
  inherit src;
  name = "confctl-m5-old-builder";
  dir = "bin";
  isExecutable = true;
  replacements = {
    ruby = pkgs.ruby;
    jsonConfig = "/var/lib/confctl/m5-paired-builder/config.json";
  };
}
