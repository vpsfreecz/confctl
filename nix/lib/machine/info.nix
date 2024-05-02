{ metaConfig, spin, name, findMetaConfig, ... }:
({ inherit name; } // findMetaConfig {
  inherit (metaConfig) cluster;
  inherit name;
})
