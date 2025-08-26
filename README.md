# O365 Mailbox Downloader

`o365mbx` is a command-line application written in Go to download emails and attachments from Microsoft Office 365 (O365) mailboxes using the Microsoft Graph API.

It is designed to be robust and efficient, with features for handling large mailboxes, large attachments, and transient network errors.

## Features

*   **Email and Attachment Download**: Downloads emails and their attachments from a specified O365 mailbox.
*   **HTML to Plain Text Conversion**: Cleans email bodies by converting HTML to plain text, preserving links and image alt text.
*   **Incremental Downloads**: Performs incremental downloads by saving the timestamp of the last run, fetching only new emails since that time.
*   **Robust Error Handling**: Implements custom error types for better error identification and handling (Authentication, API, File System).
*   **Retry Mechanism**: Includes an exponential backoff retry mechanism for transient network errors and API rate limiting.
*   **Efficient Large Attachment Handling**: Uses chunked downloads for large attachments to avoid timeouts and reduce memory usage.
*   **Parallel Downloads**: Downloads multiple messages and attachments concurrently to speed up the process, with a configurable limit.
*   **Client-Side Rate Limiting**: Proactively manages the rate of API calls to avoid hitting server-side limits.
*   **Graceful Shutdown**: Supports graceful shutdown on interrupt signals (Ctrl+C), ensuring that ongoing operations are not abruptly terminated.
*   **Flexible Configuration**: Supports configuration via both a JSON file and command-line arguments, with arguments overriding file settings.
*   **Health Check Mode**: Provides a "health check" mode to verify connectivity and authentication with the O365 mailbox without performing a full download.
*   **Structured Logging**: Uses `logrus` for structured and informative logging.

## Command-line Arguments

The application can be configured using the following command-line arguments:

| Argument      | Description                                                               | Required | Default |
|---------------|---------------------------------------------------------------------------|----------|---------|
| `-token`      | Access token for the Microsoft Graph API.                                 | **Yes**  |         |
| `-mailbox`    | The email address of the mailbox to download (e.g., `user@example.com`).  | **Yes**  |         |
| `-workspace`  | The absolute path to a unique folder for storing downloaded artifacts.    | **Yes**  |         |
| `-config`     | Path to a JSON configuration file.                                        | No       |         |
| `-timeout`    | HTTP client timeout in seconds.                                           | No       | `120`   |
| `-parallel`   | Maximum number of parallel downloads.                                     | No       | `10`    |
| `-api-rate`   | API calls per second for client-side rate limiting.                       | No       | `5.0`   |
| `-api-burst`  | API burst capacity for client-side rate limiting.                         | No       | `10`    |
| `-healthcheck`| Perform a health check on the mailbox and exit.                           | No       | `false` |
| `-version`    | Display the application version and exit.                                 | No       | `false` |

## Configuration File

For more advanced configuration, you can use a JSON file (e.g., `config.json`) and pass its path via the `-config` flag. Command-line arguments will always override the values in the configuration file.

### Example `config.json`

```json
{
  "httpClientTimeoutSeconds": 180,
  "maxRetries": 7,
  "initialBackoffSeconds": 2,
  "largeAttachmentThresholdMB": 50,
  "chunkSizeMB": 8,
  "maxParallelDownloads": 15,
  "apiCallsPerSecond": 3.0,
  "apiBurst": 6
}
```

### Configuration Directives

*   `httpClientTimeoutSeconds`: (Integer) Timeout in seconds for HTTP requests.
*   `maxRetries`: (Integer) Maximum number of retries for failed API requests.
*   `initialBackoffSeconds`: (Integer) Initial backoff duration in seconds for the retry mechanism.
*   `largeAttachmentThresholdMB`: (Integer) Attachments larger than this size (in MB) will be downloaded in chunks.
*   `chunkSizeMB`: (Integer) The size (in MB) of each chunk for large attachment downloads.
*   `maxParallelDownloads`: (Integer) The maximum number of messages to process concurrently.
*   `apiCallsPerSecond`: (Float) The number of API calls allowed per second.
*   `apiBurst`: (Integer) The burst capacity for the API rate limiter.

## Examples

### 1. Displaying the Application Version

To display the version of the application, use the `-version` flag:

```shell
./o365mbx -version
```

### 2. Running with Minimal Required Arguments

This example runs the application with only the essential arguments:

```shell
./o365mbx -token "YOUR_ACCESS_TOKEN" -mailbox "user@example.com" -workspace "/path/to/your/output"
```

### 3. Running with All Command-Line Arguments

This example demonstrates using all available command-line flags to customize behavior:

```shell
./o365mbx -token "YOUR_ACCESS_TOKEN" \
          -mailbox "user@example.com" \
          -workspace "/path/to/your/output" \
          -timeout 300 \
          -parallel 5 \
          -api-rate 2.5 \
          -api-burst 5
```

### 4. Running with a Configuration File

First, create a `config.json` file. Then, run the application pointing to this file:

```shell
./o365mbx -token "YOUR_ACCESS_TOKEN" \
          -mailbox "user@example.com" \
          -workspace "/path/to/your/output" \
          -config "/path/to/your/config.json"
```

**Note on Overrides**: If `maxParallelDownloads` is `15` in `config.json` but you specify `-parallel 5` on the command line, the application will use `5`.

## Health Check Mode

The health check mode allows you to quickly verify that the application can connect to the specified mailbox and that the access token is valid. It retrieves and displays the total message count in the inbox.

To run a health check, use the `-healthcheck` flag:

```shell
./o365mbx -token "YOUR_ACCESS_TOKEN" -mailbox "user@example.com" -healthcheck
```

## Building from Source

To build the application from source, you need to have Go installed. You can build it using the following command, which also embeds the version number:

```shell
go build -ldflags "-s -w -X main.version=$(cat version.txt)" -o o365mbx
```

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for details.
