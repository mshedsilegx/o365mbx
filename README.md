# O365 Mailbox Downloader

`o365mbx` is a command-line application written in Go to download emails and attachments from Microsoft Office 365 (O365) mailboxes using the Microsoft Graph API.

It is designed for high-performance, parallelized downloading and is robust and efficient, with features for handling large mailboxes, large attachments, and transient network errors.

## Features

*   **High-Performance Parallel Processing**: Utilizes a decoupled producer-consumer architecture to download multiple messages and attachments concurrently, maximizing throughput within configurable API limits.
*   **Efficient API Usage**: Optimizes API calls by expanding attachments directly with messages, significantly reducing network requests.
*   **Email and Attachment Download**: Downloads emails and their attachments from a specified O365 mailbox.
*   **HTML to Plain Text Conversion**: Cleans email bodies by converting HTML to plain text, preserving links and image alt text.
*   **Incremental Downloads**: Performs incremental downloads by saving the timestamp of the last run, fetching only new emails since that time.
*   **Robust Error Handling**: Implements custom error types for better error identification and handling.
*   **Configurable Retry Mechanism**: Includes an exponential backoff retry mechanism for transient network errors and API rate limiting.
*   **Efficient Large Attachment Handling**: Uses chunked downloads for large attachments to avoid timeouts and reduce memory usage.
*   **Flexible Configuration**: Supports configuration via both a JSON file and command-line arguments, with arguments overriding file settings.
*   **Bandwidth Limiting**: Allows throttling of download bandwidth to avoid hitting data egress limits on large-scale downloads.
*   **Secure Token Management**: Provides multiple, mutually exclusive options for securely supplying the access token.
*   **Health Check Mode**: Provides a "health check" mode to verify connectivity and authentication with the O365 mailbox without performing a full download.
*   **Structured Logging**: Uses `logrus` for structured and informative logging, with a configurable debug level.

## Token Management

The application requires a valid JWT access token for the Microsoft Graph API. You must provide the token using **exactly one** of the following methods:

| Flag                | Environment Variable | Description                                           |
| ------------------- | -------------------- | ----------------------------------------------------- |
| `-token-string`     |                      | Pass the token directly as a string.                  |
| `-token-file`       |                      | Provide the path to a file containing the token.      |
| `-token-env`        | `JWT_TOKEN`          | Read the token from the `JWT_TOKEN` env var.          |

For added security, you can use the `-remove-token-file` flag to automatically delete the token file after the application finishes.

## Command-line Arguments

All configuration options can be controlled via command-line arguments. Any flag you set will override the corresponding value in the configuration file.

| Argument                        | Description                                                               | Required | Default |
| ------------------------------- | ------------------------------------------------------------------------- | -------- | ------- |
| **Required**                    |                                                                           |          |         |
| `-mailbox`                      | The email address of the mailbox to download.                             | **Yes**  |         |
| `-workspace`                    | The absolute path to a unique folder for storing downloaded artifacts.    | **Yes**  |         |
| **Token (Choose One)**          |                                                                           | **Yes**  |         |
| `-token-string`                 | JWT token as a string.                                                    |          |         |
| `-token-file`                   | Path to a file containing the JWT token.                                  |          |         |
| `-token-env`                    | Read JWT token from the `JWT_TOKEN` environment variable.                 |          | `false` |
| **General**                     |                                                                           |          |         |
| `-config`                       | Path to a JSON configuration file.                                        | No       |         |
| `-debug`                        | Enable debug logging.                                                     | No       | `false` |
| `-healthcheck`                  | Perform a health check and exit.                                          | No       | `false` |
| `-version`                      | Display the application version and exit.                                 | No       | `false` |
| **Processing & State**          |                                                                           |          |         |
| `-processing-mode`              | Processing mode: `full`, `incremental`, or `route`.                       | No       | `full`  |
| `-inbox-folder`                 | The source folder from which to process messages.                         | No       | `Inbox` |
| `-state`                        | Path to the state file for incremental processing.                        | No       |         |
| `-state-save-interval`          | Save state every N messages during a run.                                 | No       | `100`   |
| `-processed-folder`             | Destination folder for successful messages in `route` mode.               | No       | `Processed`|
| `-error-folder`                 | Destination folder for failed messages in `route` mode.                   | No       | `Error` |
| **Email Body Conversion**       |                                                                           |          |         |
| `-convert-body`                 | Conversion mode for email bodies: `none`, `text`, or `pdf`.               | No       | `none`  |
| `-chromium-path`                | Absolute path to the headless Chromium/Chrome binary (required for `pdf`).| No       |         |
| **Performance & Limits**        |                                                                           |          |         |
| `-parallel`                     | Maximum number of parallel workers.                                       | No       | `10`    |
| `-timeout`                      | HTTP client timeout in seconds.                                           | No       | `120`   |
| `-max-retries`                  | Maximum number of retries for failed API calls.                           | No       | `2`     |
| `-initial-backoff-seconds`      | Initial backoff in seconds for retries.                                   | No       | `5`     |
| `-api-rate`                     | API calls per second for client-side rate limiting.                       | No       | `5.0`   |
| `-api-burst`                    | API burst capacity for client-side rate limiting.                         | No       | `10`    |
| `-bandwidth-limit-mbs`          | Bandwidth limit in MB/s for downloads (0 for disabled).                   | No       | `0.0`   |
| **Attachments**                 |                                                                           |          |         |
| `-large-attachment-threshold-mb`| Threshold in MB for an attachment to be considered "large".               | No       | `20`    |
| `-chunk-size-mb`                | Chunk size in MB for large attachment downloads.                          | No       | `8`     |
| `-remove-token-file`            | Remove the token file after use.                                          | No       | `false` |

## Configuration File

For a more permanent setup, you can use a JSON file (e.g., `config.json`) and pass its path via the `-config` flag. All options available as flags can be set in the config file.

### Example `config.json`

```json
{
  "tokenString": "your-jwt-token-here",
  "debugLogging": false,
  "processingMode": "route",
  "stateFilePath": "/path/to/your/state.json",
  "inboxFolder": "Inbox",
  "processedFolder": "Processed-Archive",
  "errorFolder": "Error-Items",
  "httpClientTimeoutSeconds": 180,
  "maxRetries": 3,
  "initialBackoffSeconds": 10,
  "largeAttachmentThresholdMB": 25,
  "chunkSizeMB": 10,
  "maxParallelDownloads": 15,
  "apiCallsPerSecond": 4.0,
  "apiBurst": 8,
  "stateSaveInterval": 50,
  "bandwidthLimitMBs": 100.0
}
```

### Configuration Directives

*   **Token Management**:
    *   `tokenString`: (String) The JWT token.
    *   `tokenFile`: (String) Path to the token file.
    *   `tokenEnv`: (Boolean) Set to `true` to use the `JWT_TOKEN` environment variable.
    *   `removeTokenFile`: (Boolean) Set to `true` to delete the token file after use.
*   **General**:
    *   `debugLogging`: (Boolean) Enables debug-level logging.
    *   `processingMode`: (String) `full`, `incremental`, or `route`. In `route` mode, messages are moved after processing.
    *   `inboxFolder`: (String) The source folder to process messages from. Defaults to the main `Inbox`.
    *   `stateFilePath`: (String) Absolute path to the state file for incremental mode.
    *   `processedFolder`: (String) The destination folder for successfully processed messages in `route` mode.
    *   `errorFolder`: (String) The destination folder for messages that failed processing in `route` mode.
*   **HTTP and API**:
    *   `httpClientTimeoutSeconds`: (Integer) Timeout in seconds for HTTP requests.
    *   `maxRetries`: (Integer) Maximum number of retries for failed API requests.
    *   `initialBackoffSeconds`: (Integer) Initial backoff duration in seconds.
    *   `maxParallelDownloads`: (Integer) The maximum number of concurrent workers.
    *   `apiCallsPerSecond`: (Float) The number of API calls allowed per second.
    *   `apiBurst`: (Integer) The burst capacity for the API rate limiter.
*   **Attachments**:
    *   `largeAttachmentThresholdMB`: (Integer) Attachments larger than this size (in MB) will be downloaded in chunks.
    *   `chunkSizeMB`: (Integer) The size (in MB) of each chunk for large attachment downloads.
    *   `bandwidthLimitMBs`: (Float) The download speed limit in megabytes per second. `0` means disabled.
*   **State**:
    *   `stateSaveInterval`: (Integer) How often to save the state file during a run (number of messages).
*   **Email Body Conversion**:
    *   `convertBody`: (String) The conversion mode for email bodies. Can be `none` (no conversion, saves `.html`), `text` (converts to plain text, saves `.txt`), or `pdf` (converts to PDF, saves `.pdf`). Defaults to `none`.
    *   `chromiumPath`: (String) The absolute path to a headless Chromium or Google Chrome binary. This is **required** if `convertBody` is set to `pdf`.

### A Note on API Permissions

For maximum security, it is recommended to use an Azure App Registration with the principle of least privilege.
*   For download-only modes (`full`, `incremental`), the `Mail.Read` permission is sufficient.
*   For `route` mode, which moves emails, the `Mail.ReadWrite` permission is required.

## Examples

### 1. Basic Run with Token String

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-string "YOUR_ACCESS_TOKEN"
```

### 2. Incremental Run Using a Token File

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -remove-token-file \
    -processing-mode incremental \
    -state "/path/to/state.json"
```

### 3. High-Performance Run Using a Config File and Overrides

This example uses a `config.json` file but overrides the parallelism and rate limits with command-line flags.

```shell
./o365mbx \
    -config "/path/to/your/config.json" \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -parallel 20 \
    -api-rate 10.0 \
    -api-burst 20
```

### 4. Route Mode with Default Folders

This example processes all messages from the "Inbox", saves the artifacts, and moves the original messages to either a "Processed" or "Error" folder in the mailbox.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-env \
    -processing-mode route
```

### 5. Route Mode with Custom Folders

This example does the same as above, but moves the messages to custom-named folders.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-env \
    -processing-mode route \
    -processed-folder "Archive/Succeeded" \
    -error-folder "Archive/Failed"
```

### 6. High-Throughput Download

This example configures the application for maximum download speed by increasing the number of parallel workers and raising the API rate limits.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -parallel 30 \
    -api-rate 15.0 \
    -api-burst 30
```

### 7. Bandwidth-Limited Download

This example throttles the total download speed to 50 MB/s to avoid hitting API data egress limits during a very large-scale download.

```shell
./o365mbx \
    -mailbox "user@example.com" \
    -workspace "/path/to/your/output" \
    -token-file "/path/to/token.txt" \
    -bandwidth-limit-mbs 50.0
```

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.
