use std::path::PathBuf;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    // Use the vendored protoc so no system protobuf compiler is required.
    let protoc = protoc_bin_vendored::protoc_bin_path()?;
    std::env::set_var("PROTOC", protoc);

    let manifest_dir = PathBuf::from(std::env::var("CARGO_MANIFEST_DIR")?);
    let proto_root = manifest_dir.join("../../proto");
    let proto_file = proto_root.join("stashfs/v1/stashfs.proto");
    println!("cargo:rerun-if-changed={}", proto_file.display());

    tonic_build::configure()
        .build_server(true)
        .build_client(true)
        .compile_protos(&[proto_file], &[proto_root])?;
    Ok(())
}
