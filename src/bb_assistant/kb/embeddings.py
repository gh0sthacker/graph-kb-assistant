"""Optional, pinned RedAmon-compatible E5 and BGE model adapters."""
from __future__ import annotations

from collections.abc import Sequence
from typing import Any


E5_MODEL = "intfloat/e5-large-v2"
E5_REVISION = "f169b11e22de13617baa190a028a32f3493550b6"
BGE_RERANKER_MODEL = "BAAI/bge-reranker-base"
BGE_RERANKER_REVISION = "2cfc18c9415c912f9d8155881c133215df768a70"


class RedAmonEmbedder:
    """E5-large-v2 wrapper using RedAmon's query/passages prefixes."""

    model_name = E5_MODEL
    dimensions = 1024

    def __init__(self, *, local_files_only: bool = True, cpu_threads: int = 1) -> None:
        if type(cpu_threads) is not int or not 1 <= cpu_threads <= 2:
            raise ValueError("cpu_threads must be between 1 and 2")
        self.local_files_only = local_files_only
        self.cpu_threads = cpu_threads
        self._model: Any = None

    def _load(self) -> Any:
        if self._model is None:
            try:
                from sentence_transformers import SentenceTransformer
            except ImportError as exc:
                raise RuntimeError(
                    "Install the optional KB model dependencies with `pip install -e '.[kb-ml]'`."
                ) from exc
            import torch

            torch.set_num_threads(self.cpu_threads)
            # Inter-op parallelism must be configured before model inference.
            torch.set_num_interop_threads(1)
            self._model = SentenceTransformer(
                self.model_name,
                revision=E5_REVISION,
                local_files_only=self.local_files_only,
            )
            get_dimensions = getattr(self._model, "get_embedding_dimension", None)
            if not callable(get_dimensions):
                get_dimensions = self._model.get_sentence_embedding_dimension
            dimensions = get_dimensions()
            if dimensions != self.dimensions:
                raise RuntimeError("The pinned E5 model returned an unexpected vector dimension.")
        return self._model

    def embed_query(self, text: str) -> list[float]:
        vector = self._load().encode(
            f"query: {text}", normalize_embeddings=True, show_progress_bar=False,
        )
        return [float(value) for value in vector]

    def embed_documents_batch(self, texts: Sequence[str], *, batch_size: int = 2) -> list[list[float]]:
        if not texts:
            return []
        if type(batch_size) is not int or not 1 <= batch_size <= 2:
            raise ValueError("batch_size must be between 1 and 2 for CPU embedding")
        vectors = self._load().encode(
            [f"passage: {text}" for text in texts],
            normalize_embeddings=True,
            batch_size=batch_size,
            show_progress_bar=False,
        )
        return [[float(value) for value in vector] for vector in vectors]


class RedAmonReranker:
    """BGE cross-encoder adapter; construction does not load model weights."""

    model_name = BGE_RERANKER_MODEL

    def __init__(self, *, local_files_only: bool = True) -> None:
        self.local_files_only = local_files_only
        self._model: Any = None

    def _load(self) -> Any:
        if self._model is None:
            try:
                from sentence_transformers import CrossEncoder
            except ImportError as exc:
                raise RuntimeError(
                    "Install the optional KB model dependencies with `pip install -e '.[kb-ml]'`."
                ) from exc
            self._model = CrossEncoder(
                self.model_name,
                revision=BGE_RERANKER_REVISION,
                local_files_only=self.local_files_only,
            )
        return self._model

    def score(self, query: str, texts: Sequence[str]) -> list[float]:
        if not texts:
            return []
        scores = self._load().predict(
            [(query[:1920], text[:1920]) for text in texts],
            batch_size=32,
            show_progress_bar=False,
        )
        return [float(value) for value in scores]
