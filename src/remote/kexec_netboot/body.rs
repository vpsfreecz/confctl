//! Private reader for this helper's Ruby Net::HTTP decoding boundary. The
//! reqwest client is raw; choose concrete gzip/zlib only after the original
//! header/Content-Range rule and prefix sniff. Every pending future owns its
//! Response; there is no self borrow, detached pump or whole-image buffer.

use async_compression::tokio::bufread::{GzipDecoder, ZlibDecoder};
use reqwest::{
    Response,
    header::{CONTENT_ENCODING, CONTENT_RANGE},
};
use std::{
    future::Future,
    io,
    pin::Pin,
    task::{Context, Poll, ready},
};
use tokio::io::{AsyncRead, AsyncReadExt, BufReader, Chain, ReadBuf};

type Chunk = Pin<Box<dyn Future<Output = (Response, io::Result<Option<Vec<u8>>>)> + Send>>;

pub(super) struct RawResponse {
    response: Option<Response>,
    pending: Option<Chunk>,
    chunk: Vec<u8>,
    offset: usize,
    eof: bool,
    failed: bool,
}

impl RawResponse {
    fn new(response: Response) -> Self {
        Self {
            response: Some(response),
            pending: None,
            chunk: Vec::new(),
            offset: 0,
            eof: false,
            failed: false,
        }
    }
}

impl AsyncRead for RawResponse {
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buffer: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        let this = self.get_mut();
        if buffer.remaining() == 0 {
            return Poll::Ready(Ok(()));
        }
        loop {
            if this.failed {
                return Poll::Ready(Err(io::Error::other("response body previously failed")));
            }
            if this.offset < this.chunk.len() {
                let count = buffer.remaining().min(this.chunk.len() - this.offset);
                buffer.put_slice(&this.chunk[this.offset..this.offset + count]);
                this.offset += count;
                return Poll::Ready(Ok(()));
            }
            if this.eof {
                return Poll::Ready(Ok(()));
            }
            this.chunk.clear();
            this.offset = 0;
            if this.pending.is_none() {
                let mut response = this.response.take().expect("owned response between chunks");
                this.pending = Some(Box::pin(async move {
                    let chunk = response
                        .chunk()
                        .await
                        .map(|chunk| chunk.map(|bytes| bytes.to_vec()))
                        .map_err(io::Error::other);
                    (response, chunk)
                }));
            }
            let (response, chunk) = ready!(
                this.pending
                    .as_mut()
                    .expect("pending chunk")
                    .as_mut()
                    .poll(cx)
            );
            this.pending = None;
            this.response = Some(response);
            match chunk {
                Ok(Some(chunk)) => this.chunk = chunk,
                Ok(None) => this.eof = true,
                Err(error) => {
                    this.failed = true;
                    return Poll::Ready(Err(error));
                }
            }
        }
    }
}

type Prefixed = BufReader<Chain<std::io::Cursor<Vec<u8>>, RawResponse>>;
pub(super) enum Body {
    Raw(RawResponse),
    Gzip(GzipDecoder<Prefixed>),
    Zlib(ZlibDecoder<Prefixed>),
}

impl AsyncRead for Body {
    fn poll_read(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
        buffer: &mut ReadBuf<'_>,
    ) -> Poll<io::Result<()>> {
        match self.get_mut() {
            Self::Raw(reader) => Pin::new(reader).poll_read(cx, buffer),
            Self::Gzip(reader) => Pin::new(reader).poll_read(cx, buffer),
            Self::Zlib(reader) => Pin::new(reader).poll_read(cx, buffer),
        }
    }
}

impl Body {
    pub(super) async fn new(response: Response) -> io::Result<Self> {
        let mut encoding = Vec::new();
        for value in response.headers().get_all(CONTENT_ENCODING).iter() {
            if !encoding.is_empty() {
                encoding.extend_from_slice(b", ");
            }
            encoding.extend_from_slice(value.as_bytes());
        }
        let recognized = [
            b"gzip".as_slice(),
            b"x-gzip".as_slice(),
            b"deflate".as_slice(),
        ]
        .iter()
        .any(|known| encoding.eq_ignore_ascii_case(known));
        let bypass = response.headers().contains_key(CONTENT_RANGE) || !recognized;
        let mut raw = RawResponse::new(response);
        if bypass {
            return Ok(Self::Raw(raw));
        }
        let mut prefix = vec![0u8; 2];
        let mut length = 0;
        while length < 2 {
            let count = raw.read(&mut prefix[length..]).await?;
            if count == 0 {
                break;
            }
            length += count;
        }
        prefix.truncate(length);
        let gzip = prefix.starts_with(&[0x1f, 0x8b]);
        let reader = BufReader::new(std::io::Cursor::new(prefix).chain(raw));
        Ok(if gzip {
            Self::Gzip(GzipDecoder::new(reader))
        } else {
            Self::Zlib(ZlibDecoder::new(reader))
        })
    }

    pub(super) async fn finish_transport(&mut self) -> io::Result<u64> {
        // A codec ending before the HTTP transport does not excuse a later
        // body/truncation error. Keep the response owned and drain to real EOF.
        // Ruby's exact trailing/multi-member policy has separate oracle gates;
        // do not equate codec defaults with that policy.
        let reader: &mut (dyn AsyncRead + Unpin + Send) = match self {
            Self::Raw(reader) => reader,
            Self::Gzip(reader) => reader.get_mut(),
            Self::Zlib(reader) => reader.get_mut(),
        };
        let mut discard = [0u8; 16 * 1024];
        let mut trailing = 0u64;
        loop {
            let count = reader.read(&mut discard).await?;
            if count == 0 {
                break;
            }
            trailing = trailing
                .checked_add(count as u64)
                .ok_or_else(|| io::Error::other("response trailing byte count overflow"))?;
        }
        Ok(trailing)
    }
}

#[cfg(test)]
#[path = "body/qualification.rs"]
mod qualification;
