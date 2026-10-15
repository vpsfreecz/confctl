These immutable output bytes come from isolated original methods of
`nix/modules/confctl/carrier/netboot/build-netboot-server.rb` (SHA256
`5faf6d76fa78e732156afeca44f95b802cc62f7798e47ac7546c2ea107d7de6d`).
The adapter never calls the original top-level entrypoint, profiles, lock,
installation or cleanup. It renders literal synthetic records matching
`originalModel` in the tests. Ruby 3.4.9 uses its default JSON 2.9.1 provider.
The scalar inputs retain object order and exact integers. Other JSON gem
providers may format floating point numbers differently. These fixtures do
not establish package, firmware, or mixed-version execution compatibility.
