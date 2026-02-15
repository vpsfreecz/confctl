{ lib, ... }:
let
  secretFile = /secrets/confctl-test/hello.txt;
in
{
  environment.etc."confctl/hello-from-secrets.txt".source = secretFile;
}
