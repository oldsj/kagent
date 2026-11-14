from buf.validate import validate_pb2 as _validate_pb2
from google.protobuf import struct_pb2 as _struct_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class StructuredObject(_message.Message):
    __slots__ = ("api_version", "kind", "value")
    API_VERSION_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    api_version: str
    kind: str
    value: _struct_pb2.Struct
    def __init__(self, api_version: _Optional[str] = ..., kind: _Optional[str] = ..., value: _Optional[_Union[_struct_pb2.Struct, _Mapping]] = ...) -> None: ...

class Workspace(_message.Message):
    __slots__ = ("repo", "ref", "branch", "depth", "service_command")
    REPO_FIELD_NUMBER: _ClassVar[int]
    REF_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    DEPTH_FIELD_NUMBER: _ClassVar[int]
    SERVICE_COMMAND_FIELD_NUMBER: _ClassVar[int]
    repo: str
    ref: str
    branch: str
    depth: int
    service_command: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, repo: _Optional[str] = ..., ref: _Optional[str] = ..., branch: _Optional[str] = ..., depth: _Optional[int] = ..., service_command: _Optional[_Iterable[str]] = ...) -> None: ...
