{ config, spin, name, findConfig, ... }:
({ inherit name; } // findConfig {
  inherit (config) cluster;
  inherit name;
})
