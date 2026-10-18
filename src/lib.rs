//! Private implementation library for confctl and its isolated acceptance tests.
//!
//! The Ruby executable remains the product entrypoint during foundation work.

pub mod app;
pub mod cli;
pub mod configuration;
pub mod domain;
pub mod error;
pub mod events;
pub mod generation;
pub mod inputs;
pub mod netboot_inventory;
pub mod nix;
pub mod operations;
pub mod presentation;
pub mod process;
pub mod remote;
pub mod runtime_kernels;
pub mod terminal;
pub mod timing;
