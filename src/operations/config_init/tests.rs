use super::*;
use crate::events::EventReceiver;
use std::{io, time::Duration};

fn fixture() -> (tempfile::TempDir, ConfigRoot, EventSink, EventReceiver) {
    let directory = tempfile::tempdir().unwrap();
    let root = ConfigRoot::new(directory.path()).unwrap();
    let (events, receiver) = EventSink::channel(1);
    (directory, root, events, receiver)
}

#[tokio::test]
async fn every_notice_is_acknowledged_before_its_effect() {
    let (_directory, root, events, mut receiver) = fixture();
    let observed = root.clone();
    let operation =
        tokio::spawn(
            async move { initialize(&root, &events, &CancellationToken::new(), create).await },
        );
    for &(path, contents) in TEMPLATES {
        let delivery = receiver.receive().await.unwrap();
        assert_eq!(
            delivery.event,
            UiEvent::Diagnostic {
                stream: OutputStream::Stdout,
                style: Style::Plain,
                text: format!(
                    "{} {path}\n",
                    if contents.is_some() {
                        "mkfile"
                    } else {
                        "mkdir"
                    }
                ),
            }
        );
        assert!(!observed.path().join(path).exists());
        assert!(!operation.is_finished());
        delivery.acknowledge(Ok(()));
    }
    operation.await.unwrap().unwrap();
    for &(path, contents) in TEMPLATES {
        if let Some(contents) = contents {
            assert_eq!(fs::read(observed.path().join(path)).unwrap(), contents);
        } else {
            assert!(observed.path().join(path).is_dir());
        }
    }
}

#[tokio::test]
async fn failed_acknowledgement_preserves_only_completed_templates() {
    let (_directory, root, events, mut receiver) = fixture();
    let observed = root.clone();
    let operation =
        tokio::spawn(
            async move { initialize(&root, &events, &CancellationToken::new(), create).await },
        );
    for _ in 0..4 {
        receiver.receive().await.unwrap().acknowledge(Ok(()));
    }
    receiver
        .receive()
        .await
        .unwrap()
        .acknowledge(Err("output fixture failed".into()));
    let failure = operation.await.unwrap().unwrap_err();
    assert!(
        matches!(&failure, ConfctlError::Cleanup { primary, secondary }
        if matches!(**primary, ConfctlError::EventDelivery(_))
        && secondary[0].to_string().contains("configs/confctl.nix; partial configuration retained"))
    );
    assert!(observed.path().join("cluster/cluster.nix").is_file());
    assert!(observed.path().join("configs").is_dir());
    assert!(!observed.path().join("configs/confctl.nix").exists());
    assert!(!observed.path().join("data").exists());
    assert!(receiver.receive().await.is_none());
}

#[tokio::test]
async fn cancellation_during_first_middle_or_final_ack_stops_before_that_effect() {
    for boundary in [0, 7, TEMPLATES.len() - 1] {
        let (_directory, root, events, mut receiver) = fixture();
        let observed = root.clone();
        let cancellation = CancellationToken::new();
        let stop = cancellation.clone();
        let operation =
            tokio::spawn(async move { initialize(&root, &events, &stop, create).await });
        for _ in 0..boundary {
            receiver.receive().await.unwrap().acknowledge(Ok(()));
        }
        let delivery = receiver.receive().await.unwrap();
        cancellation.cancel();
        delivery.acknowledge(Ok(()));
        assert!(operation.await.unwrap().unwrap_err().is_cancelled());
        assert!(!observed.path().join(TEMPLATES[boundary].0).exists());
        for &(path, _) in &TEMPLATES[..boundary] {
            assert!(observed.path().join(path).exists());
        }
        assert!(receiver.receive().await.is_none());
    }
}

#[tokio::test]
async fn a_destination_created_after_precheck_is_neither_merged_nor_overwritten() {
    for boundary in [3, 4] {
        let (_directory, root, events, mut receiver) = fixture();
        let observed = root.clone();
        let operation = tokio::spawn(async move {
            initialize(&root, &events, &CancellationToken::new(), create).await
        });
        for _ in 0..boundary {
            receiver.receive().await.unwrap().acknowledge(Ok(()));
        }
        let delivery = receiver.receive().await.unwrap();
        let destination = observed.path().join(TEMPLATES[boundary].0);
        let marker = if boundary == 3 {
            fs::create_dir(&destination).unwrap();
            destination.join("existing")
        } else {
            destination.clone()
        };
        fs::write(&marker, b"retained conflict").unwrap();
        delivery.acknowledge(Ok(()));
        let failure = operation.await.unwrap().unwrap_err();
        assert!(matches!(failure, ConfctlError::Cleanup { primary, .. }
            if matches!(&*primary, ConfctlError::Io { path: Some(path), .. } if *path == destination)));
        assert_eq!(fs::read(marker).unwrap(), b"retained conflict");
        assert_eq!(
            fs::read(observed.path().join("cluster/cluster.nix")).unwrap(),
            TEMPLATES[2].1.unwrap()
        );
        assert!(!observed.path().join("data").exists());
    }
}

#[tokio::test]
async fn partial_write_error_keeps_the_prefix_and_stops_later_effects() {
    fn short_write(
        root: &ConfigRoot,
        guard: &MutationGuard,
        path: &'static str,
        contents: Option<&[u8]>,
    ) -> Result<(), ConfctlError> {
        if path != "data/default.nix" {
            return create(root, guard, path, contents);
        }
        guard.require_root(root)?;
        let path = root.path().join(path);
        let mut file = fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o644)
            .open(&path)
            .unwrap();
        file.write_all(&contents.unwrap()[..7]).unwrap();
        Err(ConfctlError::Io {
            operation: "write init template",
            path: Some(path),
            source: io::Error::other("short write fixture"),
        })
    }
    let (_directory, root, events, mut receiver) = fixture();
    let observed = root.clone();
    let operation = tokio::spawn(async move {
        initialize(&root, &events, &CancellationToken::new(), short_write).await
    });
    while let Some(delivery) = receiver.receive().await {
        delivery.acknowledge(Ok(()));
    }
    assert!(operation.await.unwrap().is_err());
    assert_eq!(
        fs::read(observed.path().join("data/default.nix")).unwrap(),
        &TEMPLATES[6].1.unwrap()[..7]
    );
    assert!(observed.path().join("configs/confctl.nix").is_file());
    assert!(!observed.path().join("data/ssh-keys.nix").exists());
    assert!(!observed.path().join("flake.nix").exists());
}

#[tokio::test]
async fn whitelist_is_checked_again_after_waiting_for_the_same_mutation_lock() {
    let (_directory, root, events, mut receiver) = fixture();
    let guard = MutationLock::acquire(&root, &CancellationToken::new())
        .await
        .unwrap();
    let cancellation = CancellationToken::new();
    {
        let operation = initialize(&root, &events, &cancellation, create);
        tokio::pin!(operation);
        tokio::select! {
            result = &mut operation => panic!("lock was not held: {result:?}"),
            _ = tokio::time::sleep(Duration::from_millis(40)) => {},
        }
        fs::write(root.path().join("arrived-while-waiting"), b"preserved").unwrap();
        drop(guard);
        assert!(matches!(
            operation.await,
            Err(ConfctlError::Configuration(_))
        ));
    }
    assert!(!root.path().join("cluster").exists());
    drop(events);
    assert!(receiver.receive().await.is_none());
}

#[tokio::test]
async fn canceled_lock_waiter_leaves_no_templates_and_does_not_release_the_owner() {
    let (_directory, root, events, _receiver) = fixture();
    let guard = MutationLock::acquire(&root, &CancellationToken::new())
        .await
        .unwrap();
    let cancellation = CancellationToken::new();
    let operation = initialize(&root, &events, &cancellation, create);
    tokio::pin!(operation);
    tokio::select! {
        result = &mut operation => panic!("lock was not held: {result:?}"),
        _ = tokio::time::sleep(Duration::from_millis(40)) => {},
    }
    cancellation.cancel();
    assert!(operation.await.unwrap_err().is_cancelled());
    guard.require_root(&root).unwrap();
    assert!(!root.path().join("cluster").exists());
}
