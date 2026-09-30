use gateway_plugin_sdk::{ErrorCode, PluginFault};
use serde_json::{Value, json};
use std::{
    collections::HashMap,
    process::Stdio,
    sync::{
        Arc,
        atomic::{AtomicU64, Ordering},
    },
};
use tokio::{
    io::{AsyncBufReadExt, AsyncWriteExt, BufReader},
    process::{Child, Command},
    sync::{Mutex, mpsc},
};

fn fault(message: &str) -> PluginFault {
    PluginFault::new(ErrorCode::Fault, message)
}
pub struct Worker {
    tx: mpsc::Sender<Value>,
    jobs: Arc<Mutex<HashMap<String, mpsc::Sender<Value>>>>,
    next: AtomicU64,
    _child: Mutex<Child>,
    _dir: tempfile::TempDir,
}
pub struct Job {
    pub id: String,
    pub receiver: mpsc::Receiver<Value>,
    worker: Arc<Worker>,
}
impl Drop for Job {
    fn drop(&mut self) {
        let id = self.id.clone();
        let worker = self.worker.clone();
        if let Ok(handle) = tokio::runtime::Handle::try_current() {
            handle.spawn(async move {
                worker.jobs.lock().await.remove(&id);
                let _ = worker.send(json!({"op":"cancel","id":id})).await;
            });
        }
    }
}
impl Worker {
    pub async fn start() -> Result<Arc<Self>, Box<dyn std::error::Error>> {
        let dir = tempfile::Builder::new()
            .prefix(".bps-converter-")
            .tempdir_in(std::env::current_dir()?)?;
        let path = dir.path().join("bps-converter");
        tokio::fs::write(&path, include_bytes!(env!("BPS_ENGINE_BINARY"))).await?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            tokio::fs::set_permissions(&path, std::fs::Permissions::from_mode(0o700)).await?;
        }
        let mut child = Command::new(path)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::inherit())
            .kill_on_drop(true)
            .spawn()?;
        let mut stdin = child.stdin.take().ok_or("converter stdin missing")?;
        let stdout = child.stdout.take().ok_or("converter stdout missing")?;
        let mut reader = BufReader::new(stdout);
        let mut first = String::new();
        reader.read_line(&mut first).await?;
        let ready: Value = serde_json::from_str(&first)?;
        if ready["type"] != "ready" {
            return Err("converter startup failed".into());
        }
        let (tx, mut rx) = mpsc::channel::<Value>(16);
        let jobs = Arc::new(Mutex::new(HashMap::<String, mpsc::Sender<Value>>::new()));
        tokio::spawn(async move {
            while let Some(value) = rx.recv().await {
                let Ok(mut data) = serde_json::to_vec(&value) else {
                    break;
                };
                data.push(b'\n');
                if stdin.write_all(&data).await.is_err() || stdin.flush().await.is_err() {
                    break;
                }
            }
        });
        let output_jobs = jobs.clone();
        tokio::spawn(async move {
            let mut line = String::new();
            loop {
                line.clear();
                match reader.read_line(&mut line).await {
                    Ok(0) | Err(_) => break,
                    Ok(_) => {}
                };
                let Ok(value) = serde_json::from_str::<Value>(&line) else {
                    break;
                };
                let Some(id) = value["id"].as_str() else {
                    continue;
                };
                let sender = output_jobs.lock().await.get(id).cloned();
                if let Some(sender) = sender {
                    let _ = sender.send(value).await;
                }
            }
            output_jobs.lock().await.clear();
            eprintln!("BPS converter exited; plugin generation requires restart");
            std::process::exit(70);
        });
        Ok(Arc::new(Self {
            tx,
            jobs,
            next: AtomicU64::new(1),
            _child: Mutex::new(child),
            _dir: dir,
        }))
    }
    pub async fn send(&self, command: Value) -> Result<(), PluginFault> {
        self.tx
            .send(command)
            .await
            .map_err(|_| fault("converter input closed"))
    }
    pub async fn open(self: &Arc<Self>, scope: String, request: Value) -> Result<Job, PluginFault> {
        let id = self.next.fetch_add(1, Ordering::Relaxed).to_string();
        let (tx, receiver) = mpsc::channel(8);
        self.jobs.lock().await.insert(id.clone(), tx);
        let job = Job {
            id: id.clone(),
            receiver,
            worker: self.clone(),
        };
        self.send(json!({"op":"open","id":id,"scope":scope,"request":request}))
            .await?;
        Ok(job)
    }
}
