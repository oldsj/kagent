from buf.validate import validate_pb2 as _validate_pb2
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Optional as _Optional

DESCRIPTOR: _descriptor.FileDescriptor

class Workspace(_message.Message):
    __slots__ = ("repo", "ref", "branch", "depth")
    REPO_FIELD_NUMBER: _ClassVar[int]
    REF_FIELD_NUMBER: _ClassVar[int]
    BRANCH_FIELD_NUMBER: _ClassVar[int]
    DEPTH_FIELD_NUMBER: _ClassVar[int]
    repo: str
    ref: str
    branch: str
    depth: int
    def __init__(self, repo: _Optional[str] = ..., ref: _Optional[str] = ..., branch: _Optional[str] = ..., depth: _Optional[int] = ...) -> None: ...
