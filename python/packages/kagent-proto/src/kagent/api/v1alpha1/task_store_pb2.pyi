import datetime

import a2a_pb2 as _a2a_pb2
from buf.validate import validate_pb2 as _validate_pb2
from google.protobuf import timestamp_pb2 as _timestamp_pb2
from kagent.api.v1alpha1 import common_pb2 as _common_pb2
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class StoredTask(_message.Message):
    __slots__ = ("task", "version")
    TASK_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    task: _a2a_pb2.Task
    version: int
    def __init__(self, task: _Optional[_Union[_a2a_pb2.Task, _Mapping]] = ..., version: _Optional[int] = ...) -> None: ...

class TaskStoreServiceCreateTaskRequest(_message.Message):
    __slots__ = ("session_id", "task", "dispatch_id")
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    TASK_FIELD_NUMBER: _ClassVar[int]
    DISPATCH_ID_FIELD_NUMBER: _ClassVar[int]
    session_id: str
    task: _a2a_pb2.Task
    dispatch_id: str
    def __init__(self, session_id: _Optional[str] = ..., task: _Optional[_Union[_a2a_pb2.Task, _Mapping]] = ..., dispatch_id: _Optional[str] = ...) -> None: ...

class TaskStoreServiceCreateTaskResponse(_message.Message):
    __slots__ = ("version",)
    VERSION_FIELD_NUMBER: _ClassVar[int]
    version: int
    def __init__(self, version: _Optional[int] = ...) -> None: ...

class TaskStoreServiceGetTaskRequest(_message.Message):
    __slots__ = ("session_id", "task_id")
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    TASK_ID_FIELD_NUMBER: _ClassVar[int]
    session_id: str
    task_id: str
    def __init__(self, session_id: _Optional[str] = ..., task_id: _Optional[str] = ...) -> None: ...

class TaskStoreServiceGetTaskResponse(_message.Message):
    __slots__ = ("stored",)
    STORED_FIELD_NUMBER: _ClassVar[int]
    stored: StoredTask
    def __init__(self, stored: _Optional[_Union[StoredTask, _Mapping]] = ...) -> None: ...

class TaskStoreServiceUpdateTaskRequest(_message.Message):
    __slots__ = ("session_id", "task", "expected_version", "event", "dispatch_id")
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    TASK_FIELD_NUMBER: _ClassVar[int]
    EXPECTED_VERSION_FIELD_NUMBER: _ClassVar[int]
    EVENT_FIELD_NUMBER: _ClassVar[int]
    DISPATCH_ID_FIELD_NUMBER: _ClassVar[int]
    session_id: str
    task: _a2a_pb2.Task
    expected_version: int
    event: _a2a_pb2.StreamResponse
    dispatch_id: str
    def __init__(self, session_id: _Optional[str] = ..., task: _Optional[_Union[_a2a_pb2.Task, _Mapping]] = ..., expected_version: _Optional[int] = ..., event: _Optional[_Union[_a2a_pb2.StreamResponse, _Mapping]] = ..., dispatch_id: _Optional[str] = ...) -> None: ...

class TaskStoreServiceUpdateTaskResponse(_message.Message):
    __slots__ = ("version",)
    VERSION_FIELD_NUMBER: _ClassVar[int]
    version: int
    def __init__(self, version: _Optional[int] = ...) -> None: ...

class TaskStoreServiceListTasksRequest(_message.Message):
    __slots__ = ("session_id", "request")
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    REQUEST_FIELD_NUMBER: _ClassVar[int]
    session_id: str
    request: _a2a_pb2.ListTasksRequest
    def __init__(self, session_id: _Optional[str] = ..., request: _Optional[_Union[_a2a_pb2.ListTasksRequest, _Mapping]] = ...) -> None: ...

class TaskStoreServiceListTasksResponse(_message.Message):
    __slots__ = ("result",)
    RESULT_FIELD_NUMBER: _ClassVar[int]
    result: _a2a_pb2.ListTasksResponse
    def __init__(self, result: _Optional[_Union[_a2a_pb2.ListTasksResponse, _Mapping]] = ...) -> None: ...

class TaskStoreServiceSettleTaskRequest(_message.Message):
    __slots__ = ("session_id", "task_id", "version")
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    TASK_ID_FIELD_NUMBER: _ClassVar[int]
    VERSION_FIELD_NUMBER: _ClassVar[int]
    session_id: str
    task_id: str
    version: int
    def __init__(self, session_id: _Optional[str] = ..., task_id: _Optional[str] = ..., version: _Optional[int] = ...) -> None: ...

class TaskStoreServiceSettleTaskResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class TaskStoreServiceGetWorkspaceRequest(_message.Message):
    __slots__ = ("session_id",)
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    session_id: str
    def __init__(self, session_id: _Optional[str] = ...) -> None: ...

class TaskStoreServiceGetWorkspaceResponse(_message.Message):
    __slots__ = ("workspace", "preparation_required", "preparation_ready", "preparation")
    WORKSPACE_FIELD_NUMBER: _ClassVar[int]
    PREPARATION_REQUIRED_FIELD_NUMBER: _ClassVar[int]
    PREPARATION_READY_FIELD_NUMBER: _ClassVar[int]
    PREPARATION_FIELD_NUMBER: _ClassVar[int]
    workspace: _common_pb2.Workspace
    preparation_required: bool
    preparation_ready: bool
    preparation: NativeWorkspacePreparation
    def __init__(self, workspace: _Optional[_Union[_common_pb2.Workspace, _Mapping]] = ..., preparation_required: _Optional[bool] = ..., preparation_ready: _Optional[bool] = ..., preparation: _Optional[_Union[NativeWorkspacePreparation, _Mapping]] = ...) -> None: ...

class NativeWorkspacePreparation(_message.Message):
    __slots__ = ("session_id", "context_id", "create_request_id", "action_id", "request_digest", "execution_id", "challenge_id", "sequence", "generation_id", "atespace", "actor_name", "actor_uid", "prepared_revision", "workspace", "development_image", "platform", "policy_identity", "payload_image", "provider", "schema", "cli_version", "profile", "setup_digest", "config_digest", "mcp_digest")
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    CONTEXT_ID_FIELD_NUMBER: _ClassVar[int]
    CREATE_REQUEST_ID_FIELD_NUMBER: _ClassVar[int]
    ACTION_ID_FIELD_NUMBER: _ClassVar[int]
    REQUEST_DIGEST_FIELD_NUMBER: _ClassVar[int]
    EXECUTION_ID_FIELD_NUMBER: _ClassVar[int]
    CHALLENGE_ID_FIELD_NUMBER: _ClassVar[int]
    SEQUENCE_FIELD_NUMBER: _ClassVar[int]
    GENERATION_ID_FIELD_NUMBER: _ClassVar[int]
    ATESPACE_FIELD_NUMBER: _ClassVar[int]
    ACTOR_NAME_FIELD_NUMBER: _ClassVar[int]
    ACTOR_UID_FIELD_NUMBER: _ClassVar[int]
    PREPARED_REVISION_FIELD_NUMBER: _ClassVar[int]
    WORKSPACE_FIELD_NUMBER: _ClassVar[int]
    DEVELOPMENT_IMAGE_FIELD_NUMBER: _ClassVar[int]
    PLATFORM_FIELD_NUMBER: _ClassVar[int]
    POLICY_IDENTITY_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_IMAGE_FIELD_NUMBER: _ClassVar[int]
    PROVIDER_FIELD_NUMBER: _ClassVar[int]
    SCHEMA_FIELD_NUMBER: _ClassVar[int]
    CLI_VERSION_FIELD_NUMBER: _ClassVar[int]
    PROFILE_FIELD_NUMBER: _ClassVar[int]
    SETUP_DIGEST_FIELD_NUMBER: _ClassVar[int]
    CONFIG_DIGEST_FIELD_NUMBER: _ClassVar[int]
    MCP_DIGEST_FIELD_NUMBER: _ClassVar[int]
    session_id: str
    context_id: str
    create_request_id: str
    action_id: str
    request_digest: str
    execution_id: str
    challenge_id: str
    sequence: int
    generation_id: str
    atespace: str
    actor_name: str
    actor_uid: str
    prepared_revision: str
    workspace: _common_pb2.Workspace
    development_image: str
    platform: str
    policy_identity: str
    payload_image: str
    provider: str
    schema: int
    cli_version: str
    profile: str
    setup_digest: str
    config_digest: str
    mcp_digest: str
    def __init__(self, session_id: _Optional[str] = ..., context_id: _Optional[str] = ..., create_request_id: _Optional[str] = ..., action_id: _Optional[str] = ..., request_digest: _Optional[str] = ..., execution_id: _Optional[str] = ..., challenge_id: _Optional[str] = ..., sequence: _Optional[int] = ..., generation_id: _Optional[str] = ..., atespace: _Optional[str] = ..., actor_name: _Optional[str] = ..., actor_uid: _Optional[str] = ..., prepared_revision: _Optional[str] = ..., workspace: _Optional[_Union[_common_pb2.Workspace, _Mapping]] = ..., development_image: _Optional[str] = ..., platform: _Optional[str] = ..., policy_identity: _Optional[str] = ..., payload_image: _Optional[str] = ..., provider: _Optional[str] = ..., schema: _Optional[int] = ..., cli_version: _Optional[str] = ..., profile: _Optional[str] = ..., setup_digest: _Optional[str] = ..., config_digest: _Optional[str] = ..., mcp_digest: _Optional[str] = ...) -> None: ...

class TaskStoreServiceCompleteWorkspacePreparationRequest(_message.Message):
    __slots__ = ("session_id", "assignment", "head", "branch", "transport_digest", "native_hook", "observed_at", "confirmed")
    SESSION_ID_FIELD_NUMBER: _ClassVar[int]
    ASSIGNMENT_FIELD_NUMBER: _ClassVar[int]
    HEAD_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    TRANSPORT_DIGEST_FIELD_NUMBER: _ClassVar[int]
    NATIVE_HOOK_FIELD_NUMBER: _ClassVar[int]
    OBSERVED_AT_FIELD_NUMBER: _ClassVar[int]
    CONFIRMED_FIELD_NUMBER: _ClassVar[int]
    session_id: str
    assignment: NativeWorkspacePreparation
    head: str
    branch: str
    transport_digest: str
    native_hook: str
    observed_at: _timestamp_pb2.Timestamp
    confirmed: bool
    def __init__(self, session_id: _Optional[str] = ..., assignment: _Optional[_Union[NativeWorkspacePreparation, _Mapping]] = ..., head: _Optional[str] = ..., branch: _Optional[str] = ..., transport_digest: _Optional[str] = ..., native_hook: _Optional[str] = ..., observed_at: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., confirmed: _Optional[bool] = ...) -> None: ...

class TaskStoreServiceCompleteWorkspacePreparationResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...
