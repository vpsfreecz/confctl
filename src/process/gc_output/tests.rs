use super::*;
#[tokio::test]
async fn both_streams_keep_bounded_fragments_bytes_and_shared_monotone_activity() {
    let output = GcOutput::new("node");
    let (events, mut receiver) = EventSink::channel(1);
    let cancel = CancellationToken::new();
    let consumer = tokio::spawn(async move {
        let mut observed = Vec::new();
        while let Some(delivery) = receiver.receive().await {
            let (event, ack) = delivery.into_parts();
            observed.push(event);
            ack.acknowledge(Ok(()));
        }
        observed
    });
    let mut overlong = b"deleting '/nix/store/".to_vec();
    overlong.extend(vec![b'x'; LINE_LIMIT + 10]);
    overlong.extend(b"'\n");
    let a = async {
        output
            .forward(
                &events,
                (TaskId(1), OutputStream::Stdout),
                b"delet",
                false,
                &cancel,
            )
            .await
            .unwrap();
        output
            .forward(
                &events,
                (TaskId(1), OutputStream::Stdout),
                b"ing '/nix/store/a'\nordinary\xff\n2 store paths deleted, native summary\n",
                false,
                &cancel,
            )
            .await
            .unwrap();
        output
            .forward(
                &events,
                (TaskId(1), OutputStream::Stdout),
                &overlong,
                true,
                &cancel,
            )
            .await
            .unwrap();
    };
    let b = async {
        output
            .forward(
                &events,
                (TaskId(1), OutputStream::Stderr),
                b"deleting '/nix/store/b'\nnot deleting '/nix/store/c'\ntail\0\xff",
                true,
                &cancel,
            )
            .await
            .unwrap();
    };
    tokio::join!(a, b);
    assert!(
        output
            .summary()
            .await
            .unwrap()
            .starts_with("2 store paths deleted")
    );
    drop(events);
    let observed = consumer.await.unwrap();
    let counts = observed
        .iter()
        .filter_map(|event| {
            if let UiEvent::TaskProgress {
                completed, total, ..
            } = event
            {
                assert!(total.is_none());
                Some(*completed)
            } else {
                None
            }
        })
        .collect::<Vec<_>>();
    assert_eq!(counts, [1, 2]);
    let lines = observed
        .iter()
        .filter_map(|event| {
            if let UiEvent::Output { bytes, .. } = event {
                assert!(bytes.starts_with(b"node> "));
                assert!(bytes.len() <= LINE_LIMIT + 30);
                Some(bytes.as_slice())
            } else {
                None
            }
        })
        .collect::<Vec<_>>();
    assert!(
        lines
            .iter()
            .any(|b| b.windows(9).any(|w| w == b"ordinary\xff"))
    );
    assert!(lines.iter().any(|b| b.starts_with(b"node> [continued] ")));
    assert!(lines.iter().any(|b| b.ends_with(b"tail\0\xff\n")));
}
#[tokio::test]
async fn cancellation_after_output_ack_is_not_a_progress_success() {
    let output = GcOutput::new("node");
    let (events, mut receiver) = EventSink::channel(1);
    let cancel = CancellationToken::new();
    let pending = output.forward(
        &events,
        (TaskId(1), OutputStream::Stdout),
        b"deleting '/nix/store/a'\n",
        false,
        &cancel,
    );
    tokio::pin!(pending);
    let mut cx = std::task::Context::from_waker(std::task::Waker::noop());
    assert!(std::future::Future::poll(pending.as_mut(), &mut cx).is_pending());
    let delivery = receiver.receive().await.unwrap();
    cancel.cancel();
    delivery.acknowledge(Ok(()));
    assert!(pending.await.unwrap_err().is_cancelled());
    assert_eq!(output.state.lock().await.completed, 0);
}
