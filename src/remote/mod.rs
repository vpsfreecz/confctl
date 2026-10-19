//! Concrete remote helper implementations and their preserved data formatting.
//! No generic remote task protocol or extension mechanism lives here.

#[cfg(test)]
mod helper_diagnostic_receipts;

pub(crate) mod json;

pub mod auto_rollback;
pub mod carrier_env;
pub mod kexec_netboot;
pub mod netboot_server;
mod ruby_integer;
