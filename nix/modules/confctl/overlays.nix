{ config, ... }:
{
  nixpkgs.overlays = [
    (self: super: {
      # nixos-unstable removed pkgs.substituteAll, but nixos-25.05 does not
      # include the new function pkgs.replaceVarsWith
      confReplaceVarsWith =
        { replacements, ... } @ args:
        if builtins.hasAttr "replaceVarsWith" self then
          self.replaceVarsWith args
        else
          self.substituteAll ((builtins.removeAttrs args [ "replacements" ]) // replacements);
    })
  ];
}