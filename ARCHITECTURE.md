# o365mbx Application Architecture

The project `o365mbx` is a Go command-line application designed to download emails and attachments from Microsoft Office 365 mailboxes using the Microsoft Graph API. It prioritizes high-performance, concurrency, and robust error handling.

## Core Architecture and Components:

1.  `main` package:
2.  `engine` package:
3.  `o365client` package:
4.  `filehandler` package:
5.  `emailprocessor` package:
6.  `apperrors` package:

### Overall Strengths:
*   **Modularity and Separation of Concerns:** The codebase is well-organized into distinct packages, each with a clear responsibility.
*   **Robustness:** Extensive error handling, retry mechanisms with exponential backoff and jitter, and client-side rate limiting contribute to a resilient application.
*   **Concurrency:** Effective use of Go's concurrency features for high-performance operations.
*   **Security:** Attention to detail in workspace creation (TOCTOU, symlink checks) and filename sanitization.
*   **Flexibility:** Comprehensive configuration options via flags and JSON file.
*   **Efficiency:** Optimized Graph API calls, chunked downloads for large attachments, and incremental processing.

## Performance and Parallelism Logic:

The application leverages Go's concurrency features (goroutines, channels, and `sync.WaitGroup`) to implement a highly parallelized producer-consumer architecture for efficient email and attachment downloading.

Here's a breakdown of the parallel logic:

1.  **Main Orchestration** (`engine.RunEngine` and `runDownloadMode`):
2.  **Producer** (Single Goroutine):
3.  **Processors** (Multiple Goroutines):
4.  **Downloaders** (Multiple Goroutines):
5.  **Aggregator** (Single Goroutine, only in `route` mode):

Messages are fetched concurrently by the producer. These messages are then processed in parallel by multiple processor goroutines. If messages have attachments, these are then downloaded in parallel by multiple downloader goroutines. All these operations are rate-limited and respect context cancellation for graceful shutdown. In "route" mode, a dedicated aggregator goroutine tracks the overall progress of each message and performs the final move operation in O365. This decoupled, concurrent design allows the application to maximize throughput and efficiently handle large volumes of data and API interactions.

## Parallelism Tuning:

### Definition
The `parallel` flag in `o365mbx` controls the `MaxParallelDownloads` configuration setting.

### Role of the `-parallel` flag:
**Concurrency Limit:** It determines the maximum number of concurrent workers (goroutines) that will process messages and download attachments simultaneously.

1.  **Command-line flag:**
    ```bash
    ./o365mbx -mailbox "user@example.com" -workspace "/path/to/output" -token-env -parallel 20
    ```

2.  **Configuration file (JSON):**
    You can include `maxParallelDownloads` in your JSON configuration file.

    Example `config.json` snippet:
    ```json
    {
      "mailboxName": "user@example.com",
      "workspacePath": "/path/to/your/output",
      "maxParallelDownloads": 15,
      "apiCallsPerSecond": 4.0
    }
    ```
    Then, run the application referencing this config file:
    ```bash
    ./o365mbx -config "/path/to/your/config.json"
    ```

### Recommendation:
*   The default value is 10.

## How to Adjust to Not Trigger O365 Graph API Throttling:

Adjusting the `-parallel` flag to avoid O365 Graph API throttling requires a holistic approach, considering not just the number of concurrent operations but also the rate at which API requests are made.

Here are the best practices:

1.  **Understand Graph API Throttling:**
2.  **The Role of `-parallel` in Throttling:**
3.  **Key Configuration Flags for Throttling Prevention:**
    *   **Start Conservatively:** Begin with lower values for both `-parallel` and `-api-rate`. The default values (`-parallel 10`, `-api-rate 5.0`, `-api-burst 10`) are a good starting point.
    *   **Initial Run:** `./o365mbx -parallel 10 -api-rate 5.0 -api-burst 10 ...`

By systematically adjusting these parameters and closely monitoring the application's behavior and logs, you can find an optimal balance that maximizes download speed while minimizing the risk of O365 Graph API throttling.