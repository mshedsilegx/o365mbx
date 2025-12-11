# System Visualizations

This document contains visual representations of the `o365mbx` application's architecture, flow, and data structures. These diagrams are generated using Mermaid syntax.

## 1. Package Dependency Map

This diagram illustrates the high-level architecture and how the different Go packages within the project interact. It highlights the central role of the `main` package in initialization and the `engine` package in orchestrating the core logic.

*   **Core**: The `main` entry point and the `engine` which drives the application.
*   **Services**: specialized modules for API interaction (`o365client`), file system operations (`filehandler`), and content processing (`emailprocessor`).
*   **Utils**: Shared utilities like error definitions.

```mermaid
graph TD
    subgraph Core
        Main[main]
        Engine[engine]
    end

    subgraph Services
        O365Client[o365client]
        FileHandler[filehandler]
        EmailProcessor[emailprocessor]
        Presenter[presenter]
    end

    subgraph Utils
        AppErrors[apperrors]
    end

    Main -->|Initializes & Configures| Engine
    Main -->|Initializes| O365Client
    Main -->|Initializes| FileHandler
    Main -->|Initializes| EmailProcessor
    Main -->|Uses for Healthcheck| Presenter

    Engine -->|Orchestrates| O365Client
    Engine -->|Orchestrates| FileHandler
    Engine -->|Orchestrates| EmailProcessor

    FileHandler -->|Uses| O365Client
    FileHandler -->|Uses| EmailProcessor
    
    O365Client -->|Returns Errors| AppErrors
    Engine -->|Returns Errors| AppErrors
```

## 2. Concurrency Model (Producer-Consumer)

This sequence diagram details the application's "heartbeat" — the parallel processing pipeline. It visualizes how the application maximizes throughput using Go's concurrency primitives.

*   **Producer**: Fetches messages from O365.
*   **Processors (W1)**: A pool of workers that process message bodies and discover attachments.
*   **Downloaders (W2)**: A separate pool of workers dedicated to downloading attachments.
*   **Aggregator**: (Route Mode only) Tracks completion of all parts of a message (body + attachments) before moving it to a destination folder.

```mermaid
sequenceDiagram
    participant P as Producer (O365Client)
    participant C1 as Messages Channel
    participant W1 as Processor Workers
    participant C2 as Attachments Channel
    participant W2 as Downloader Workers
    participant C3 as Results Channel
    participant A as Aggregator

    Note over P, A: Parallel Execution Flow

    P->>C1: Send Message (models.Messageable)
    loop Parallel Processors
        W1->>C1: Read Message
        W1->>W1: Process Body & Save Metadata
        alt Has Attachments
            W1->>P: Fetch Attachment List (API Call)
            P-->>W1: Return List
            loop For Each Attachment
                W1->>C2: Send AttachmentJob
            end
        end
        W1->>C3: Send ProcessingResult (Body)
    end

    loop Parallel Downloaders
        W2->>C2: Read AttachmentJob
        W2->>W2: Save Attachment File
        W2->>C3: Send ProcessingResult (Attachment)
    end

    loop Aggregator (Route Mode Only)
        A->>C3: Read ProcessingResult
        A->>A: Update Message State
        opt All Tasks Complete
            A->>P: Move Message (Processed/Error)
        end
    end
```

## 3. Data Flow & Channel Structures

This diagram focuses on *what* data moves through the system. It maps the Go structs to the channels that transport them, providing a clear view of the data pipeline.

*   **messagesChan**: Carries `models.Messageable` objects from the API.
*   **attachmentsChan**: Carries `AttachmentJob` structs created by processors for downloaders.
*   **resultsChan**: Carries `ProcessingResult` structs to the aggregator for final status tracking.

```mermaid
graph LR
    subgraph Sources
        API[Microsoft Graph API]
    end

    subgraph Channels
        MsgChan[[messagesChan]]
        AttChan[[attachmentsChan]]
        ResChan[[resultsChan]]
    end

    subgraph Structs
        MsgStruct(models.Messageable)
        JobStruct(AttachmentJob)
        ResStruct(ProcessingResult)
    end

    API -->|Fetches| MsgStruct
    MsgStruct --> MsgChan
    MsgChan -->|Consumed by| Processor[Processor Goroutine]
    
    Processor -->|Creates| JobStruct
    JobStruct --> AttChan
    AttChan -->|Consumed by| Downloader[Downloader Goroutine]

    Processor -->|Creates| ResStruct
    Downloader -->|Creates| ResStruct
    
    ResStruct --> ResChan
    ResChan -->|Consumed by| Aggregator[Aggregator Goroutine]
```
